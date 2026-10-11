#!/usr/bin/env python3

import argparse
import json
import math
import os
import statistics
import sys

import numpy as np
import pandas as pd

_HERE = os.path.dirname(os.path.abspath(__file__))
_BACKTEST = os.path.abspath(os.path.join(_HERE, "..", ".."))
_REPO = os.path.abspath(os.path.join(_BACKTEST, ".."))
sys.path.insert(0, _BACKTEST)
sys.path.insert(0, os.path.join(_REPO, "shared_tools"))

import offline_manifest as om
from backtester import aggregate_close_validations
from eval_windows import INCUMBENTS, evaluate_window, expand_sweep, manifest_datasets
from optimizer import DEFAULT_PARAM_RANGES

ISSUE = 1658
MANIFEST = os.path.join(_HERE, "study_manifest.json")
STRESS_MANIFEST = os.path.join(_HERE, "study_manifest_fee_x2.json")
CANDIDATE = "money_flow_index_reversal"
COMPARATORS = ("rsi", "mean_reversion_pro")
ARMS = (CANDIDATE,) + COMPARATORS
CLOSE_STRATEGIES = [{"name": "tiered_tp_atr",
                     "params": {"tp_tiers": [{"atr_multiple": 2.0, "close_fraction": 1.0}]}}]
COMPARISON_MODE = None
STOP_LOSS_ATR_MULT = 1.0
DIRECTION = "both"
REGIME = {"allowed_regimes": ["ranging"], "regime_period": 14, "regime_adx_threshold": 20.0,
          "regime_gate_on_failure": "closed"}
CAPITAL = 1000.0
SELECTION_WINDOW = "train"
HELD_OUT_WINDOW = "test"
BASE_COST = 1.0
STRESS_COST = 2.0
MIN_TRAIN_CLUSTERS = 10
MIN_TEST_CLUSTERS = 30
MIN_SIDE_POSITIONS = 10
MAX_WORST_DD_PCT = 20.0
HOUR_MS = 3_600_000
SOURCE_FILES = (
    "shared_strategies/open/money_flow_index.py",
    "shared_strategies/open/mean_reversion_pro.py",
    "shared_strategies/open/registry.py",
    "shared_strategies/close/tiered_tp_atr.py",
    "shared_tools/strategy_composition.py",
    "shared_tools/regime.py",
    "backtest/backtester.py",
    "backtest/eval_windows.py",
    "backtest/offline_manifest.py",
    "backtest/optimizer.py",
    "backtest/candidates/money_flow_index_reversal_1658/run_study.py",
)


def _sha(path: str) -> str:
    return om.sha256_file(path)


def arm_candidate(name: str, params: dict, regime: bool = True) -> dict:
    cand = {
        "name": name,
        "params": dict(params),
        "direction": DIRECTION,
        "close_strategies": json.loads(json.dumps(CLOSE_STRATEGIES)),
        "stop_loss_atr_mult": STOP_LOSS_ATR_MULT,
        "comparison_mode": COMPARISON_MODE,
    }
    if regime:
        cand.update(json.loads(json.dumps(REGIME)))
    return cand


def _grid(name: str, defaults: dict) -> list:
    specs = [(k, list(v)) for k, v in DEFAULT_PARAM_RANGES[name].items()]
    return expand_sweep(dict(defaults), specs)


def _window_preflight(manifest: dict, ds: dict, window_name: str) -> dict:
    out = {"manifest": os.path.relpath(manifest["path"], _REPO), "dataset": ds["key"],
           "window": window_name, "problems": []}
    win = manifest["windows"][window_name]
    step = manifest["interval_ms"]
    try:
        frame, _, cov = om.window_frame(manifest, ds, window_name)
    except om.ManifestError as exc:
        out["problems"].append(f"candles: {exc}")
        out["complete"] = False
        return out
    out["candle_coverage"] = cov
    ts = frame["timestamp"].to_numpy(dtype=np.int64)
    expected_rows = manifest["warmup_bars"] + cov["expected_bars"]
    if not cov["complete"] or cov["warmup_missing_bars"]:
        out["problems"].append(
            f"candles: {cov['missing_bars']} window bars and {cov['warmup_missing_bars']} warm-up bars missing")
    if len(ts) != expected_rows or (len(ts) > 1 and not bool((np.diff(ts) == step).all())):
        out["problems"].append(f"candles: {len(ts)} rows off the exact {manifest['interval']} grid "
                               f"(expected {expected_rows})")
    in_window = ts[(ts >= win["start_ms"]) & (ts < win["end_ms"])]
    if len(in_window) == 0 or int(in_window[0]) != win["start_ms"] or int(in_window[-1]) != win["end_ms"] - step:
        out["problems"].append("candles: window edges do not match the window start and end")
    o, h, lo, c, v = (frame[k].to_numpy(dtype=float) for k in ("open", "high", "low", "close", "volume"))
    with np.errstate(invalid="ignore"):
        prices_ok = (np.isfinite(o) & np.isfinite(h) & np.isfinite(lo) & np.isfinite(c)
                     & (o > 0) & (h > 0) & (lo > 0) & (c > 0) & (h >= lo) & (c <= h) & (c >= lo))
        volume_ok = np.isfinite(v) & (v >= 0)
    bad_price = int((~prices_ok).sum())
    bad_volume = int((~volume_ok).sum())
    out["invalid_price_rows"] = bad_price
    out["invalid_volume_rows"] = bad_volume
    if bad_price:
        out["problems"].append(f"candles: {bad_price} rows with invalid prices")
    if bad_volume:
        out["problems"].append(f"candles: {bad_volume} rows with missing, non-finite or negative volume")
    funding = om.load_funding(ds)
    fcov = om.funding_coverage(funding, win["start_ms"], win["end_ms"])
    out["funding_coverage"] = fcov
    if not fcov["complete"]:
        out["problems"].append(f"funding: {fcov['missing_hours']} hourly records missing (left-closed window)")
    first_slot = (win["start_ms"] - step) // HOUR_MS
    last_slot = (win["end_ms"] - step) // HOUR_MS
    have = set() if funding is None else set((funding["timestamp"].to_numpy(dtype=np.int64) // HOUR_MS).tolist())
    missing_slots = [s for s in range(first_slot, last_slot + 1) if s not in have]
    out["accrual_span"] = {
        "first_slot_ms": first_slot * HOUR_MS, "last_slot_ms": last_slot * HOUR_MS,
        "expected_slots": last_slot - first_slot + 1, "missing_slots": len(missing_slots),
        "first_missing_ms": missing_slots[0] * HOUR_MS if missing_slots else None,
    }
    if missing_slots:
        out["problems"].append(f"funding: {len(missing_slots)} hourly records missing in the right-closed accrual span")
    out["complete"] = not out["problems"]
    return out


def preflight(manifests: list) -> dict:
    rows = []
    for manifest in manifests:
        for ds in manifest["datasets"]:
            for window_name in (SELECTION_WINDOW, HELD_OUT_WINDOW):
                rows.append(_window_preflight(manifest, ds, window_name))
    refused = [{"manifest": r["manifest"], "dataset": r["dataset"], "window": r["window"],
                "problems": r["problems"]} for r in rows if not r["complete"]]
    return {"complete": not refused, "refused": refused, "windows": rows}


def clusters(positions: list) -> int:
    spans = sorted((pd.Timestamp(p["entry_date"]), pd.Timestamp(p["exit_date"])) for p in positions)
    count = 0
    latest_exit = None
    for entry, exit_ in spans:
        if latest_exit is None or entry > latest_exit:
            count += 1
            latest_exit = exit_
        else:
            latest_exit = max(latest_exit, exit_)
    return count


def summarize(score: dict, window_bars: dict, expected: int) -> dict:
    rows = [r for r in score["rows"] if r["leg"] is not None]
    if not rows:
        return {"datasets": 0, "expected_datasets": expected, "positions": 0, "clusters": 0}
    legs = [r["leg"] for r in rows]
    positions = []
    per_dataset = {}
    for r in rows:
        leg = r["leg"]
        ex = leg["execution"]
        per_dataset[r["dataset"]] = {
            "return_pct": leg["return_pct"],
            "sharpe": leg["sharpe"],
            "max_dd_pct": leg["max_dd_pct"],
            "ddadj": leg["ddadj"],
            "liquidated": bool(leg.get("liquidated")),
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
            "candle_coverage_complete": leg["manifest"]["candle_coverage"]["complete"],
            "funding_coverage_complete": leg["manifest"]["funding_coverage"]["complete"],
        }
        for p in ex["position_list"]:
            positions.append(dict(p, dataset=r["dataset"]))
    abs_ds = [abs(v["net_pnl_usd"]) for v in per_dataset.values()]
    abs_pos = sorted((abs(p["net_pnl"]) for p in positions), reverse=True)
    exit_reasons = {}
    for p in positions:
        for reason in p["exit_reasons"]:
            key = (reason or "").split(":")[0] or "unknown"
            exit_reasons[key] = exit_reasons.get(key, 0) + 1
    return {
        "datasets": len(rows),
        "expected_datasets": expected,
        "mean_return_pct": round(statistics.mean(l["return_pct"] for l in legs), 4),
        "mean_sharpe": round(statistics.mean(l["sharpe"] for l in legs), 4),
        "mean_ddadj": round(statistics.mean(l["ddadj"] for l in legs), 4),
        "worst_max_dd_pct": round(max(abs(l["max_dd_pct"]) for l in legs), 4),
        "net_pnl_usd": round(sum(v["net_pnl_usd"] for v in per_dataset.values()), 4),
        "fees_usd": round(sum(v["fees_usd"] for v in per_dataset.values()), 4),
        "funding_pnl_usd": round(sum(v["funding_pnl_usd"] for v in per_dataset.values()), 4),
        "mean_turnover": round(statistics.mean(v["turnover"] for v in per_dataset.values()), 4),
        "positions": len(positions),
        "long_positions": sum(1 for p in positions if p["side"] == "long"),
        "short_positions": sum(1 for p in positions if p["side"] == "short"),
        "clusters": clusters(positions),
        "exit_reasons": dict(sorted(exit_reasons.items())),
        "liquidated_legs": sum(1 for l in legs if l.get("liquidated")),
        "top_dataset_abs_pnl_share": (round(max(abs_ds) / sum(abs_ds), 4) if sum(abs_ds) > 0 else None),
        "top5_position_abs_pnl_share": (round(sum(abs_pos[:5]) / sum(abs_pos), 4) if sum(abs_pos) > 0 else None),
        "coverage_complete": all(v["candle_coverage_complete"] and v["funding_coverage_complete"]
                                 for v in per_dataset.values()),
        "incumbent_m1_verdict": score.get("verdict"),
        "per_dataset": per_dataset,
        "close_validation": score.get("close_validation"),
    }


def eligible(summary: dict) -> bool:
    return (summary["datasets"] == summary["expected_datasets"]
            and summary.get("coverage_complete", False)
            and summary.get("liquidated_legs", 1) == 0
            and summary["clusters"] >= MIN_TRAIN_CLUSTERS)


def select(rows: list) -> dict:
    pool = [(i, r) for i, r in enumerate(rows) if eligible(r["summary"])]
    if not pool:
        return {"rule_outcome": "no_eligible_combo", "selected": None, "params": None,
                "eligible_combos": 0}
    best = max(pool, key=lambda kv: (kv[1]["summary"]["mean_sharpe"],
                                     kv[1]["summary"]["mean_ddadj"], -kv[0]))[1]
    return {"rule_outcome": "selected", "selected": best["label"], "params": best["params"],
            "eligible_combos": len(pool)}


def neighbors(params: dict, rows: list, name: str) -> dict:
    ranges = DEFAULT_PARAM_RANGES[name]
    near = []
    for r in rows:
        diffs = [abs(list(grid).index(params[k]) - list(grid).index(r["params"][k]))
                 for k, grid in ranges.items()]
        if sum(diffs) == 1:
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


def verdict(gate: dict, arms: dict, held: dict) -> dict:
    live_status = ("edge_status=no_edge regardless of outcome; paper needs explicit --mode=paper, "
                   "live needs allow_no_edge: true; no acknowledgement, promotion or deployment")
    if not gate["complete"]:
        return {"outcome": "inconclusive", "live_status": live_status, "checks": {"coverage_complete": False},
                "reason": "coverage gate refused " + ", ".join(
                    f"{r['dataset']} {r['window']} ({os.path.basename(r['manifest'])})" for r in gate["refused"])}
    unselected = [n for n in ARMS if arms[n]["selection"]["selected"] is None]
    if unselected:
        return {"outcome": "inconclusive", "live_status": live_status,
                "checks": {"coverage_complete": True, "every_arm_selected": False},
                "reason": "no eligible training selection for " + ", ".join(unselected)}
    cand = held[CANDIDATE]["selected"]["base"]
    stress = held[CANDIDATE]["selected"]["stress"]
    comps = {c: held[c]["selected"]["base"] for c in COMPARATORS}
    checks = {
        "coverage_complete": True,
        "every_arm_selected": True,
        "clusters_sufficient": cand["clusters"] >= MIN_TEST_CLUSTERS,
        "long_positions_sufficient": cand["long_positions"] >= MIN_SIDE_POSITIONS,
        "short_positions_sufficient": cand["short_positions"] >= MIN_SIDE_POSITIONS,
    }
    if not all(checks.values()):
        short = sorted(k for k, ok in checks.items() if not ok)
        return {"outcome": "inconclusive", "live_status": live_status, "checks": checks,
                "reason": (f"held-out sample too small ({cand['clusters']} independent clusters, "
                           f"{cand['long_positions']} long and {cand['short_positions']} short positions; "
                           f"need {MIN_TEST_CLUSTERS} clusters and {MIN_SIDE_POSITIONS} per side): "
                           + ", ".join(short))}
    checks.update({
        "net_positive_base": cand["net_pnl_usd"] > 0,
        "net_positive_stress": stress["net_pnl_usd"] > 0,
        "no_liquidation": cand["liquidated_legs"] == 0 and stress["liquidated_legs"] == 0,
        "worst_drawdown_within_limit": cand["worst_max_dd_pct"] <= MAX_WORST_DD_PCT,
        "beats_comparators_sharpe": all(cand["mean_sharpe"] > c["mean_sharpe"] for c in comps.values()),
        "beats_comparators_ddadj": all(cand["mean_ddadj"] > c["mean_ddadj"] for c in comps.values()),
    })
    if all(checks.values()):
        return {"outcome": "pass", "live_status": live_status, "checks": checks,
                "reason": "every frozen held-out check passed"}
    failed = sorted(k for k, ok in checks.items() if not ok)
    return {"outcome": "fail", "live_status": live_status, "checks": checks,
            "reason": "failed frozen checks: " + ", ".join(failed)}


def _manifest_record(manifest: dict) -> dict:
    return {
        "path": os.path.relpath(manifest["path"], _REPO),
        "sha256": _sha(manifest["path"]),
        "study": manifest["study"],
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
    }


def _protocol() -> dict:
    return {
        "close_strategies": CLOSE_STRATEGIES,
        "comparison_mode": COMPARISON_MODE or "strict",
        "stop_loss_atr_mult": STOP_LOSS_ATR_MULT,
        "direction": DIRECTION,
        "regime": REGIME,
        "capital": CAPITAL,
        "selection_window": SELECTION_WINDOW,
        "held_out_window": HELD_OUT_WINDOW,
        "selection_rule": (
            "eligible = complete coverage, all datasets scored, no liquidated leg, >= "
            f"{MIN_TRAIN_CLUSTERS} independent trade clusters; pick max mean net Sharpe, then max mean "
            "drawdown-adjusted return, then earliest grid order; no eligible combo = no selection"),
        "cost_runs": {"base": {"manifest": "study_manifest.json", "cost_multiplier": BASE_COST},
                      "stress": {"manifest": "study_manifest_fee_x2.json", "cost_multiplier": STRESS_COST}},
        "min_test_clusters": MIN_TEST_CLUSTERS,
        "min_side_positions": MIN_SIDE_POSITIONS,
        "max_worst_dd_pct": MAX_WORST_DD_PCT,
        "incumbents_reference": list(INCUMBENTS),
    }


def run(manifest_path: str, stress_path: str) -> dict:
    from registry_loader import load_registry
    manifest = om.load_manifest(manifest_path)
    stress_manifest = om.load_manifest(stress_path)
    if [d["key"] for d in manifest["datasets"]] != [d["key"] for d in stress_manifest["datasets"]] or any(
            a["candles"]["sha256"] != b["candles"]["sha256"]
            or (a["funding"] or {}).get("sha256") != (b["funding"] or {}).get("sha256")
            for a, b in zip(manifest["datasets"], stress_manifest["datasets"])):
        raise om.ManifestError("stress manifest must pin the same datasets and hashes as the base manifest")
    gate = preflight([manifest, stress_manifest])
    result = {
        "study": manifest["study"],
        "issue": ISSUE,
        "manifest": _manifest_record(manifest),
        "stress_manifest": _manifest_record(stress_manifest),
        "source_sha256": {p: _sha(os.path.join(_REPO, p)) for p in SOURCE_FILES},
        "protocol": _protocol(),
        "coverage_gate": gate,
    }
    if not gate["complete"]:
        result["verdict"] = verdict(gate, {}, {})
        return result

    reg = load_registry("futures")
    datasets = manifest_datasets(manifest)
    window_bars = {}
    for ds in manifest["datasets"]:
        for w in (SELECTION_WINDOW, HELD_OUT_WINDOW):
            _, _, cov = om.window_frame(manifest, ds, w)
            window_bars.setdefault(w, {})[ds["key"]] = cov["present_bars"]
    memo: dict = {}

    def score(name, params, window, cost="base", regime=True):
        m, mult = (manifest, BASE_COST) if cost == "base" else (stress_manifest, STRESS_COST)
        s = evaluate_window(reg, arm_candidate(name, params, regime=regime), datasets, window, CAPITAL, memo,
                            manifest=m, cost_multiplier=mult)
        return summarize(s, window_bars[window], len(datasets))

    arms = {}
    for name in ARMS:
        defaults = dict(reg.STRATEGY_REGISTRY[name]["default_params"])
        train_rows = []
        for label, params in _grid(name, defaults):
            summary = score(name, params, SELECTION_WINDOW)
            summary.pop("per_dataset", None)
            train_rows.append({"label": label, "params": params, "summary": summary})
        choice = select(train_rows)
        arms[name] = {
            "defaults": defaults,
            "grid": {k: list(v) for k, v in DEFAULT_PARAM_RANGES[name].items()},
            "train_sweep": train_rows,
            "selection": choice,
            "selected_plateau": neighbors(choice["params"], train_rows, name) if choice["selected"] else None,
        }

    held = {}
    for name in ARMS:
        params = arms[name]["selection"]["params"]
        entry = {"selected": None}
        if params is not None:
            entry["selected"] = {
                "params": params,
                "base": score(name, params, HELD_OUT_WINDOW),
                "stress": score(name, params, HELD_OUT_WINDOW, cost="stress"),
            }
        descriptive = params if params is not None else arms[name]["defaults"]
        entry["descriptive"] = {
            "params": descriptive,
            "params_source": "selected" if params is not None else "registry defaults (no selection)",
            "regime_gated_base": (entry["selected"]["base"] if params is not None
                                  else score(name, descriptive, HELD_OUT_WINDOW)),
            "ungated_base": score(name, descriptive, HELD_OUT_WINDOW, regime=False),
        }
        held[name] = entry

    variants = []
    for row in arms[CANDIDATE]["train_sweep"]:
        test = score(CANDIDATE, row["params"], HELD_OUT_WINDOW)
        variants.append({"label": row["label"], "params": row["params"],
                         "train_eligible": eligible(row["summary"]),
                         "train_mean_sharpe": row["summary"].get("mean_sharpe"),
                         "train_clusters": row["summary"]["clusters"],
                         "test_mean_sharpe": test.get("mean_sharpe"),
                         "test_net_pnl_usd": test.get("net_pnl_usd"),
                         "test_positions": test["positions"],
                         "test_clusters": test["clusters"]})
    scored = [v for v in variants if v["train_mean_sharpe"] is not None and v["test_mean_sharpe"] is not None]
    stability = {
        "spearman_train_vs_test_sharpe": spearman([v["train_mean_sharpe"] for v in scored],
                                                  [v["test_mean_sharpe"] for v in scored]),
        "test_net_positive_variants": sum(1 for v in variants if (v["test_net_pnl_usd"] or 0) > 0),
        "train_eligible_variants": sum(1 for v in variants if v["train_eligible"]),
        "variants": len(variants),
    }

    validations = []
    for entry in held.values():
        for s in ((entry["selected"] or {}).get("base"), (entry["selected"] or {}).get("stress"),
                  entry["descriptive"]["ungated_base"]):
            if s is not None:
                validations.append(s.get("close_validation"))
    result.update({
        "arms": arms,
        "held_out": held,
        "candidate_variants_held_out": variants,
        "parameter_stability": stability,
        "verdict": verdict(gate, arms, held),
        "close_validation": aggregate_close_validations(validations),
    })
    return result


def _fmt(v, nd=2):
    if v is None:
        return "-"
    if isinstance(v, bool):
        return "yes" if v else "no"
    if isinstance(v, float):
        if not math.isfinite(v):
            return str(v)
        return f"{v:.{nd}f}"
    return str(v)


def _summary_row(name: str, label: str, s: dict) -> str:
    if not s or not s.get("datasets"):
        return f"| `{name}` | {label} | no scored dataset |" + " |" * 11
    return (f"| `{name}` | {label} | {_fmt(s['net_pnl_usd'])} | {_fmt(s['mean_return_pct'])} | "
            f"{_fmt(s['mean_sharpe'], 3)} | {_fmt(s['mean_ddadj'], 3)} | {_fmt(s['worst_max_dd_pct'])} | "
            f"{_fmt(s['fees_usd'])} | {_fmt(s['funding_pnl_usd'])} | {_fmt(s['mean_turnover'])} | "
            f"{s['positions']} ({s['long_positions']}/{s['short_positions']}) | {s['clusters']} | "
            f"{_fmt(s['top_dataset_abs_pnl_share'])} | {_fmt(s['top5_position_abs_pnl_share'])} |")


def render(result: dict) -> str:
    v = result["verdict"]
    m = result["manifest"]
    lines = [
        "# Money Flow Index reversals: held-out result (#1658)",
        "",
        f"Verdict: **{v['outcome'].upper()}**. {v['reason']}.",
        "",
        f"Availability: {v['live_status']}.",
        "",
        "Generated by `run_study.py` from `results.json`; do not edit by hand. The frozen specification "
        "is in `README.md`.",
        "",
        "## Inputs",
        "",
        f"- Base manifest `{m['path']}` sha256 `{m['sha256']}`",
        f"- Stress manifest `{result['stress_manifest']['path']}` sha256 `{result['stress_manifest']['sha256']}` "
        f"(fees taker {result['stress_manifest']['costs']['taker_fee_pct']*100:.3f}%, maker "
        f"{result['stress_manifest']['costs']['maker_fee_pct']*100:.3f}%)",
        f"- Provenance: {m['provenance']['kind']}: {m['provenance']['label']} (native venue volume; no proxy)",
        f"- Interval {m['interval']}, warm-up {m['warmup_bars']} bars (not scored)",
    ]
    for name, w in sorted(m["windows"].items(), key=lambda kv: kv[1]["start"]):
        lines.append(f"- Window `{name}` [{w['start']}, {w['end']}): {w['role']}")
    c = m["costs"]
    lines.append(
        f"- Base costs: taker {c['taker_fee_pct']*100:.3f}%, maker {c['maker_fee_pct']*100:.3f}%, slippage "
        f"{c['slippage_bps']} bps per side plus per-coin half spread, minimum order ${c['min_notional_usd']:.0f} "
        f"with a {c['min_notional_margin']*100:.0f}% margin, funding booked hourly")
    for d in m["datasets"]:
        lines.append(f"- `{d['key']}`: szDecimals {d['size_decimals']}, half spread {d['half_spread_bps']} bps, "
                     f"candles `{d['candles_sha256'][:16]}`, funding `{(d['funding_sha256'] or '')[:16]}`")
    gate = result["coverage_gate"]
    lines += ["", "## Coverage gate", "",
              f"Complete: {_fmt(gate['complete'])}. Windows checked: {len(gate['windows'])}.", ""]
    for r in gate["refused"]:
        lines.append(f"- REFUSED {r['dataset']} {r['window']} ({r['manifest']}): {'; '.join(r['problems'])}")
    if gate["complete"]:
        lines.append("Every candle grid, row, funding window and accrual span check passed before selection.")
    p = result["protocol"]
    lines += [
        "",
        "## Protocol",
        "",
        f"- Every arm: direction `{p['direction']}`, close owner `tiered_tp_atr` "
        f"(`{json.dumps(p['close_strategies'][0]['params'], sort_keys=True)}`), comparison mode "
        f"`{p['comparison_mode']}`, stop owner fixed entry ATR x{p['stop_loss_atr_mult']}, capital "
        f"${p['capital']:.0f}, fills at the next bar open.",
        f"- Regime gate: `{json.dumps(p['regime'], sort_keys=True)}` (existing ADX classifier, prior-bar timing).",
        f"- Selection: {p['selection_rule']}; scored on `{p['selection_window']}` only.",
        f"- Held-out `{p['held_out_window']}`: base run on the base manifest at cost x1; stress run on the "
        "fee x2 manifest at cost x2 (fees, spread and slippage all doubled).",
    ]
    if "arms" not in result:
        lines += ["", "No selection or score was computed because the coverage gate refused a window.", ""]
        return "\n".join(lines)
    lines += ["", "## Selections (train)", "",
              "| Arm | Eligible combos | Selected params | Train mean Sharpe | Train clusters | Neighbour median Sharpe |",
              "|---|---|---|---|---|---|"]
    for name in ARMS:
        arm = result["arms"][name]
        sel = arm["selection"]
        row = next((r for r in arm["train_sweep"] if r["label"] == sel.get("selected")), None)
        plateau = arm["selected_plateau"] or {}
        lines.append(f"| `{name}` | {sel['eligible_combos']}/{len(arm['train_sweep'])} | "
                     f"{('`' + json.dumps(sel['params'], sort_keys=True) + '`') if sel['params'] else 'none'} | "
                     f"{_fmt(row['summary']['mean_sharpe'], 3) if row else '-'} | "
                     f"{row['summary']['clusters'] if row else '-'} | "
                     f"{_fmt(plateau.get('neighbor_median_sharpe'), 3)} |")
    header = ("| Arm | Run | Net $ | Mean ret % | Mean Sharpe | Mean DDadj | Worst DD % | Fees $ | Funding $ | "
              "Turnover | Positions (L/S) | Clusters | Top dataset share | Top-5 share |")
    sep = "|---|---|---|---|---|---|---|---|---|---|---|---|---|---|"
    lines += ["", "## Held-out comparison (verdict inputs)", "", header, sep]
    for name in ARMS:
        t = result["held_out"][name]
        if t["selected"] is None:
            lines.append(f"| `{name}` | no selection |" + " |" * 12)
            continue
        lines.append(_summary_row(name, "selected x1", t["selected"]["base"]))
        lines.append(_summary_row(name, "selected stress", t["selected"]["stress"]))
    lines += ["", "## Descriptive sensitivity (not a verdict input)", "",
              "The same arms on the held-out window with the regime gate removed, beside the gated run. "
              "These rows cannot change any selection or the verdict.", "", header, sep]
    for name in ARMS:
        d = result["held_out"][name]["descriptive"]
        lines.append(_summary_row(name, f"gated, {d['params_source']}", d["regime_gated_base"]))
        lines.append(_summary_row(name, "ungated, same params", d["ungated_base"]))
    sel_c = result["held_out"][CANDIDATE]["selected"]
    if sel_c is not None:
        lines += ["", "## Candidate per dataset (held-out, selected, cost x1)", "",
                  "| Dataset | Net $ | Ret % | Sharpe | Max DD % | Fees $ | Funding $ | Positions (L/S) | "
                  "Time in market | Rejected entries |",
                  "|---|---|---|---|---|---|---|---|---|---|"]
        for ds, s in sorted(sel_c["base"]["per_dataset"].items()):
            lines.append(f"| {ds} | {_fmt(s['net_pnl_usd'])} | {_fmt(s['return_pct'])} | {_fmt(s['sharpe'], 3)} | "
                         f"{_fmt(s['max_dd_pct'])} | {_fmt(s['fees_usd'])} | {_fmt(s['funding_pnl_usd'])} | "
                         f"{s['positions']} ({s['long_positions']}/{s['short_positions']}) | "
                         f"{_fmt(s['time_in_market'])} | {s['rejected_entries']} |")
    lines += ["", "## Verdict checks", ""]
    for k, ok in sorted(v["checks"].items()):
        lines.append(f"- {k}: {_fmt(ok)}")
    st = result["parameter_stability"]
    lines += [
        "",
        "## Parameter stability and unsuccessful variants",
        "",
        f"- Training-eligible candidate variants: {st['train_eligible_variants']}/{st['variants']}",
        f"- Spearman rank correlation of train vs held-out mean Sharpe: "
        f"{_fmt(st['spearman_train_vs_test_sharpe'], 3)}",
        f"- Held-out net-positive variants: {st['test_net_positive_variants']}/{st['variants']}",
        "",
        "| Variant | Train eligible | Train Sharpe | Train clusters | Held-out Sharpe | Held-out net $ | "
        "Held-out positions | Held-out clusters |",
        "|---|---|---|---|---|---|---|---|",
    ]
    for row in result["candidate_variants_held_out"]:
        lines.append(f"| {row['label']} | {_fmt(row['train_eligible'])} | {_fmt(row['train_mean_sharpe'], 3)} | "
                     f"{row['train_clusters']} | {_fmt(row['test_mean_sharpe'], 3)} | "
                     f"{_fmt(row['test_net_pnl_usd'])} | {row['test_positions']} | {row['test_clusters']} |")
    cv = result.get("close_validation") or {}
    lines += ["", "## Close validation", "",
              f"- Modes `{', '.join(cv.get('modes') or [str(cv.get('mode'))])}`, eligibility "
              f"`{cv.get('close_eligibility')}`, parity status `{cv.get('parity_status')}`, incomplete parity "
              f"{_fmt(cv.get('incomplete_parity'))}.",
              "- `unverified` is the strict, eligible capability status (no approximation and no refusal). "
              "It is not a ledger-verified claim that these exits equal live exits.",
              "", "## Source hashes", ""]
    for path, digest in sorted(result["source_sha256"].items()):
        lines.append(f"- `{path}` `{digest}`")
    lines.append("")
    return "\n".join(lines)


def main(argv=None) -> int:
    p = argparse.ArgumentParser(description="#1658 frozen held-out study driver (suggest-only)")
    p.add_argument("--manifest", default=MANIFEST)
    p.add_argument("--stress-manifest", default=STRESS_MANIFEST)
    p.add_argument("--json", default=os.path.join(_HERE, "results.json"), dest="json_out")
    p.add_argument("--report", default=os.path.join(_HERE, "REPORT.md"))
    p.add_argument("--render-only", action="store_true",
                   help="Re-render the report from the results JSON without rerunning the study")
    args = p.parse_args(argv)
    if args.render_only:
        with open(args.json_out) as fh:
            result = json.load(fh)
    else:
        try:
            result = run(args.manifest, args.stress_manifest)
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

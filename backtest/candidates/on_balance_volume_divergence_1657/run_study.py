#!/usr/bin/env python3

import argparse
import copy
import hashlib
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
from backtester import aggregate_close_validations
from eval_windows import INCUMBENTS, evaluate_window, expand_sweep, manifest_datasets
from optimizer import DEFAULT_PARAM_RANGES
from parity_diff import ParityConfig, compute_parity_frame
from parity_diff import summarize as parity_summarize

ISSUE = 1657
TRACKING_GROUP = "hyperliquid-strategy-candidates-2026-10-02"
MANIFEST = os.path.join(_HERE, "study_manifest.json")
CANDIDATE_SEED = os.path.join(_HERE, "candidate_seed.json")
BASELINE_SEED = os.path.join(_HERE, "baseline_seed.json")
CANDIDATE = "on_balance_volume_divergence"
BASELINE = "volume_weighted"
ABLATION_OVERRIDE = {"volume_test": False}
CAPITAL = 1000.0
SELECTION_WINDOW = "train"
HELD_OUT_WINDOW = "test"
SELECTION_DIRECTION = "both"
BASE_COST = 1.0
STRESS_COST = 2.0
MIN_TRAIN_POSITIONS = 10
MIN_HELD_OUT_CLUSTERS = 30
PRIMARY_ARMS = ("candidate_both", "candidate_long")
PARITY_RUNS = ((None, False), (200, False), (200, True))
FIXTURE_SEED = 1657
EDGE_SOURCE_BY_OUTCOME = {"pass": "unvalidated", "fail": "study_fail", "inconclusive": "study_inconclusive"}
SOURCE_FILES = (
    "shared_strategies/open/on_balance_volume.py",
    "shared_strategies/open/registry.py",
    "shared_tools/strategy_composition.py",
    "shared_strategies/close/tiered_tp_pct.py",
    "backtest/backtester.py",
    "backtest/eval_windows.py",
    "backtest/offline_manifest.py",
    "backtest/optimizer.py",
    "backtest/parity_diff.py",
    "backtest/candidates/on_balance_volume_divergence_1657/run_study.py",
    "backtest/candidates/on_balance_volume_divergence_1657/candidate_seed.json",
    "backtest/candidates/on_balance_volume_divergence_1657/baseline_seed.json",
)
SELECTION_RULE = (
    f"candidate only, direction {SELECTION_DIRECTION}, cost x{BASE_COST}, window {SELECTION_WINDOW}: eligible = "
    "every dataset scored, candle and funding coverage complete, no incomplete-funding leg, no liquidated leg, "
    f">= {MIN_TRAIN_POSITIONS} positions; pick max mean Sharpe, then max mean drawdown-adjusted return, "
    "then earliest optimizer-grid order; no eligible combination = inconclusive")
VERDICT_RULE = (
    "inconclusive when evidence is incomplete (candle or funding coverage, incomplete-funding leg, proxy-only data), "
    "no train combination is eligible, or a primary arm (candidate both, candidate long) has fewer than "
    f"{MIN_HELD_OUT_CLUSTERS} independent held-out position clusters; otherwise pass only if both primary arms are "
    "net positive at cost x1 and x2, candidate long beats ablation long and baseline long on mean net return "
    "with a worst drawdown no deeper than each, and candidate both beats ablation both on mean net return; "
    "else fail")


def _sha(path: str) -> str:
    return om.sha256_file(path)


def _load_json(path: str) -> dict:
    with open(path) as fh:
        return json.load(fh)


def _close_config(seed: dict) -> dict:
    return {k: copy.deepcopy(seed[k]) for k in ("close_strategies", "stop_loss_atr_mult", "comparison_mode")}


def load_seeds() -> tuple:
    cand, base = _load_json(CANDIDATE_SEED), _load_json(BASELINE_SEED)
    if cand["name"] != CANDIDATE or base["name"] != BASELINE:
        raise SystemExit("seed files name the wrong strategies")
    if _close_config(cand) != _close_config(base):
        raise SystemExit("candidate and baseline seeds must share close, stop and comparison mode")
    if cand["direction"] != "both" or base["direction"] != "long":
        raise SystemExit("candidate seed must be bidirectional and the baseline seed long-only")
    return cand, base


def protocol(cand: dict, base: dict) -> dict:
    return {
        "issue": ISSUE,
        "tracking_group": TRACKING_GROUP,
        "candidate_seed": cand,
        "baseline_seed": base,
        "candidate_grid": {k: list(v) for k, v in DEFAULT_PARAM_RANGES[CANDIDATE].items()},
        "ablation_override": ABLATION_OVERRIDE,
        "capital": CAPITAL,
        "selection_window": SELECTION_WINDOW,
        "held_out_window": HELD_OUT_WINDOW,
        "selection_rule": SELECTION_RULE,
        "verdict_rule": VERDICT_RULE,
        "cost_multipliers": {"base": BASE_COST, "stress": STRESS_COST},
        "min_train_positions": MIN_TRAIN_POSITIONS,
        "min_held_out_clusters": MIN_HELD_OUT_CLUSTERS,
        "primary_arms": list(PRIMARY_ARMS),
        "parity_runs": [{"window": w, "batched": b} for w, b in PARITY_RUNS],
        "fixture_seed": FIXTURE_SEED,
        "intrabar_resolution": "ohlc_walk",
        "funding": "manifest frozen funding attachment, charged hourly",
    }


def protocol_sha256(proto: dict) -> str:
    return hashlib.sha256(json.dumps(proto, sort_keys=True, separators=(",", ":")).encode()).hexdigest()


def arm_spec(seed: dict, name: str, params: dict, direction: str) -> dict:
    spec = _close_config(seed)
    spec.update({"name": name, "params": dict(params), "direction": direction})
    return spec


def _grid(defaults: dict) -> list:
    specs = [(k, list(v)) for k, v in DEFAULT_PARAM_RANGES[CANDIDATE].items()]
    return expand_sweep(dict(defaults), specs)


def position_clusters(positions: list) -> int:
    spans = sorted((pd.Timestamp(p["entry_date"]), pd.Timestamp(p["exit_date"])) for p in positions)
    count, end = 0, None
    for s, e in spans:
        if end is None or s > end:
            count += 1
            end = e
        else:
            end = max(end, e)
    return count


def summarize(score: dict, window_bars: dict) -> dict:
    rows = [r for r in score["rows"] if r["leg"] is not None]
    if not rows:
        return {"datasets": 0, "positions": 0, "clusters": 0, "coverage_complete": False,
                "funding_incomplete_legs": 0, "liquidated_legs": 0}
    legs = [r["leg"] for r in rows]
    positions = []
    per_dataset = {}
    for r in rows:
        leg, ex = r["leg"], r["leg"]["execution"]
        per_dataset[r["dataset"]] = {
            "return_pct": leg["return_pct"],
            "sharpe": leg["sharpe"],
            "max_dd_pct": leg["max_dd_pct"],
            "ddadj": leg["ddadj"],
            "net_pnl_usd": ex["net_pnl_usd"],
            "fees_usd": ex["fees_usd"],
            "funding_pnl_usd": ex["funding_pnl_usd"],
            "turnover": ex["turnover"],
            "positions": ex["positions"],
            "long_positions": ex["long_positions"],
            "short_positions": ex["short_positions"],
            "time_in_market": round(ex["bars_in_market"] / window_bars[r["dataset"]], 4),
            "rejected_entries": ex.get("rejected_entries", 0),
            "candle_coverage": leg["manifest"]["candle_coverage"],
            "funding_coverage": leg["manifest"]["funding_coverage"],
            "funding_incomplete": bool(leg.get("funding_incomplete")),
            "beats_incumbent_sharpe": r["beats_sharpe"],
            "beats_incumbent_ddadj": r["beats_ddadj"],
        }
        for p in ex["position_list"]:
            positions.append(dict(p, dataset=r["dataset"]))
    abs_ds = [abs(v["net_pnl_usd"]) for v in per_dataset.values()]
    abs_pos = sorted((abs(p["net_pnl"]) for p in positions), reverse=True)
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
        "clusters": position_clusters(positions),
        "liquidated_legs": sum(1 for l in legs if l.get("liquidated")),
        "funding_incomplete_legs": sum(1 for v in per_dataset.values() if v["funding_incomplete"]),
        "top_dataset_abs_pnl_share": (round(max(abs_ds) / sum(abs_ds), 4) if sum(abs_ds) > 0 else None),
        "top5_position_abs_pnl_share": (round(sum(abs_pos[:5]) / sum(abs_pos), 4) if sum(abs_pos) > 0 else None),
        "coverage_complete": all(v["candle_coverage"]["complete"] and v["funding_coverage"]["complete"]
                                 for v in per_dataset.values()),
        "incumbent_m1_verdict": score.get("verdict"),
        "per_dataset": per_dataset,
        "positions_list": positions,
        "close_validation": score.get("close_validation"),
    }


def _brief(summary: dict, drop=("per_dataset", "positions_list")) -> dict:
    return {k: v for k, v in summary.items() if k not in drop}


def eligible(summary: dict, expected: int) -> bool:
    return (summary["datasets"] == expected and summary["coverage_complete"]
            and summary["funding_incomplete_legs"] == 0 and summary["liquidated_legs"] == 0
            and summary["positions"] >= MIN_TRAIN_POSITIONS)


def select(rows: list, expected: int) -> dict:
    ok = [(i, r) for i, r in enumerate(rows) if eligible(r["summary"], expected)]
    if not ok:
        return {"rule_outcome": "no_eligible_combo", "selected": None, "eligible": 0}
    best = max(ok, key=lambda kv: (kv[1]["summary"]["mean_sharpe"], kv[1]["summary"]["mean_ddadj"], -kv[0]))[1]
    return {"rule_outcome": "selected", "selected": best["label"], "params": best["params"], "eligible": len(ok)}


def neighbors(params: dict, rows: list) -> dict:
    ranges = DEFAULT_PARAM_RANGES[CANDIDATE]
    near = []
    for r in rows:
        diffs = [abs(grid.index(params[k]) - grid.index(r["params"][k])) for k, grid in ranges.items()]
        if sum(diffs) == 1:
            near.append(r["summary"]["mean_sharpe"])
    return {"neighbor_count": len(near),
            "neighbor_median_sharpe": round(statistics.median(near), 4) if near else None,
            "neighbor_min_sharpe": round(min(near), 4) if near else None,
            "neighbor_positive_sharpe": sum(1 for v in near if v > 0)}


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


def verdict(held_out: dict, selection: dict, evidence: dict) -> dict:
    base = {k: v["base"] for k, v in held_out.items()}
    stress = {k: v["stress"] for k, v in held_out.items()}
    evidence_ok = (not evidence["proxy_only"]
                   and all(s["coverage_complete"] and s["funding_incomplete_legs"] == 0
                           for arm in held_out.values() for s in arm.values()))
    sample_ok = all(base[a]["clusters"] >= MIN_HELD_OUT_CLUSTERS for a in PRIMARY_ARMS)
    cl, al, bl = base["candidate_long"], base["ablation_long"], base["baseline_long"]
    checks = {
        "evidence_complete": evidence_ok,
        "selection_eligible": selection.get("selected") is not None,
        "sample_floor_met": sample_ok,
        "candidate_both_net_positive_x1": base["candidate_both"]["mean_return_pct"] > 0,
        "candidate_both_net_positive_x2": stress["candidate_both"]["mean_return_pct"] > 0,
        "candidate_long_net_positive_x1": cl["mean_return_pct"] > 0,
        "candidate_long_net_positive_x2": stress["candidate_long"]["mean_return_pct"] > 0,
        "long_beats_ablation_return": cl["mean_return_pct"] > al["mean_return_pct"],
        "long_beats_baseline_return": cl["mean_return_pct"] > bl["mean_return_pct"],
        "long_drawdown_not_worse_than_ablation": cl["worst_max_dd_pct"] >= al["worst_max_dd_pct"],
        "long_drawdown_not_worse_than_baseline": cl["worst_max_dd_pct"] >= bl["worst_max_dd_pct"],
        "both_beats_ablation_return": base["candidate_both"]["mean_return_pct"]
        > base["ablation_both"]["mean_return_pct"],
    }
    if not evidence_ok:
        outcome, reason = "inconclusive", "venue, cost or funding evidence is incomplete or proxy-only"
    elif not checks["selection_eligible"]:
        outcome, reason = "inconclusive", "no train-window combination met the frozen eligibility rule"
    elif not sample_ok:
        counts = ", ".join(f"{a} {base[a]['clusters']}" for a in PRIMARY_ARMS)
        outcome, reason = "inconclusive", (
            f"independent held-out position clusters below {MIN_HELD_OUT_CLUSTERS} ({counts})")
    elif all(checks.values()):
        outcome, reason = "pass", "every frozen held-out check passed"
    else:
        outcome, reason = "fail", "failed frozen checks: " + ", ".join(sorted(k for k, v in checks.items() if not v))
    return {"outcome": outcome, "reason": reason, "checks": checks,
            "edge_source": EDGE_SOURCE_BY_OUTCOME[outcome],
            "availability": ("edge_status no_edge in every outcome: hidden from discovery, explicit paper mode "
                             "runs it, live use still needs allow_no_edge: true; a research pass approves nothing")}


def verdict_guards(held_out: dict, selection: dict, evidence: dict) -> dict:
    funding_gap = copy.deepcopy(held_out)
    funding_gap["candidate_both"]["base"]["funding_incomplete_legs"] = 1
    coverage_gap = copy.deepcopy(held_out)
    coverage_gap["baseline_long"]["base"]["coverage_complete"] = False
    proxy = dict(evidence, proxy_only=True)
    thin = copy.deepcopy(held_out)
    thin["candidate_long"]["base"]["clusters"] = MIN_HELD_OUT_CLUSTERS - 1
    out = {
        "funding_incomplete": verdict(funding_gap, selection, evidence)["outcome"],
        "coverage_incomplete": verdict(coverage_gap, selection, evidence)["outcome"],
        "proxy_only": verdict(held_out, selection, proxy)["outcome"],
        "below_sample_floor": verdict(thin, selection, evidence)["outcome"],
        "no_selection": verdict(held_out, {"selected": None}, evidence)["outcome"],
    }
    if any(v == "pass" for v in out.values()):
        raise SystemExit(f"verdict guard failed: {out}")
    return out


def frame_sha256(frame: pd.DataFrame) -> str:
    cols = ["timestamp", "open", "high", "low", "close", "volume"]
    return hashlib.sha256(frame[cols].to_csv(index=False).encode()).hexdigest()


SIDE_DIAGNOSTICS = ("pivot1_ts_ms", "pivot1_price", "pivot2_ts_ms", "pivot2_price", "obv1", "obv2",
                    "signed_sum", "abs_sum", "divergence", "reclaim", "confirm_ts_ms", "expiry_ts_ms")


def _ms(value):
    return None if value != value else str(pd.to_datetime(int(value), unit="ms"))


def entry_audit(reg, manifest: dict, params: dict, positions: list) -> list:
    frames = {}
    for ds in manifest["datasets"]:
        frame, _, _ = om.window_frame(manifest, ds, HELD_OUT_WINDOW)
        frames[ds["key"]] = (frame, reg.apply_strategy(CANDIDATE, frame, dict(params)))
    rows = []
    for p in sorted(positions, key=lambda x: (x["dataset"], x["entry_date"])):
        frame, out = frames[p["dataset"]]
        fill = out.index.get_loc(pd.Timestamp(p["entry_date"]))
        sig_row = fill - 1
        rec = out.iloc[sig_row]
        side = p["side"]
        support = int(rec["obvd_support_bars"])
        rows.append({
            "dataset": p["dataset"],
            "side": side,
            "signal_bar": str(out.index[sig_row]),
            "fill_bar": str(out.index[fill]),
            "exit_bar": p["exit_date"],
            "exit_reasons": p.get("exit_reasons"),
            "net_pnl_usd": p["net_pnl"],
            "signal_at_signal_bar": int(rec["signal"]),
            "reason": rec["obvd_reason"],
            "support_bars": support,
            "support_first_bar": str(out.index[sig_row - support + 1]),
            "support_sha256": frame_sha256(frame.iloc[sig_row - support + 1:sig_row + 1]),
            "diagnostics": {k: (None if rec[f"obvd_{side}_{k}"] != rec[f"obvd_{side}_{k}"]
                                else float(rec[f"obvd_{side}_{k}"])) for k in SIDE_DIAGNOSTICS},
            "pivot_bars": [_ms(rec[f"obvd_{side}_pivot1_ts_ms"]), _ms(rec[f"obvd_{side}_pivot2_ts_ms"])],
            "confirm_bar": _ms(rec[f"obvd_{side}_confirm_ts_ms"]),
            "expiry_bar": _ms(rec[f"obvd_{side}_expiry_ts_ms"]),
        })
    return rows


def frozen_parity(manifest: dict, seed: dict, params: dict) -> list:
    rows = []
    for ds in manifest["datasets"]:
        frame, _, _ = om.window_frame(manifest, ds, HELD_OUT_WINDOW)
        for window, batched in PARITY_RUNS:
            if batched:
                cfg = ParityConfig(strategy_name=CANDIDATE, params=dict(params), registry="futures",
                                   batched=True, symbol=ds["coin"], timeframe=manifest["interval"])
            else:
                cfg = ParityConfig(strategy_name=CANDIDATE, params=dict(params), registry="futures",
                                   close_refs=copy.deepcopy(seed["close_strategies"]), direction="both",
                                   comparison_mode=seed["comparison_mode"])
            parity = compute_parity_frame(frame, cfg=cfg, window=window)
            summary = parity_summarize(parity)
            rows.append({
                "dataset": ds["key"],
                "window": "every prefix" if window is None else window,
                "batched": batched,
                "input_sha256": frame_sha256(frame),
                "frame_bars": len(frame),
                "compared_bars": int(len(parity)),
                "mismatches": int((~parity["match"]).sum()),
                "entry_decisions": int((parity["live_open_action"] != "none").sum()),
                "decision_agreement": summary["decision_agreement"],
                "close_parity": summary.get("close_parity"),
                "clean": summary.get("clean"),
            })
    return rows


def run(manifest_path: str) -> dict:
    from registry_loader import load_registry
    cand_seed, base_seed = load_seeds()
    proto = protocol(cand_seed, base_seed)
    manifest = om.load_manifest(manifest_path)
    reg = load_registry("futures")
    datasets = manifest_datasets(manifest)
    expected = len(datasets)
    window_bars = {}
    for ds in manifest["datasets"]:
        for w in (SELECTION_WINDOW, HELD_OUT_WINDOW):
            _, _, cov = om.window_frame(manifest, ds, w)
            window_bars.setdefault(w, {})[ds["key"]] = cov["present_bars"]
    memo: dict = {}

    def score(seed, name, params, direction, window, cost):
        s = evaluate_window(reg, arm_spec(seed, name, params, direction), datasets, window, CAPITAL, memo,
                            manifest=manifest, cost_multiplier=cost)
        return summarize(s, window_bars[window])

    defaults = dict(reg.STRATEGY_REGISTRY[CANDIDATE]["default_params"])
    if defaults != cand_seed["params"]:
        raise SystemExit("candidate seed params must equal the registry defaults")
    train_rows = []
    for label, params in _grid(defaults):
        summary = _brief(score(cand_seed, CANDIDATE, params, SELECTION_DIRECTION, SELECTION_WINDOW, BASE_COST),
                         drop=("per_dataset", "positions_list", "close_validation"))
        train_rows.append({"label": label, "params": params, "summary": summary})
    selection = select(train_rows, expected)
    selected = selection["params"] if selection["selected"] else defaults
    ablation = dict(selected, **ABLATION_OVERRIDE)
    baseline = dict(base_seed["params"])

    arms = {
        "candidate_both": (cand_seed, CANDIDATE, selected, "both"),
        "ablation_both": (cand_seed, CANDIDATE, ablation, "both"),
        "candidate_long": (cand_seed, CANDIDATE, selected, "long"),
        "ablation_long": (cand_seed, CANDIDATE, ablation, "long"),
        "baseline_long": (base_seed, BASELINE, baseline, "long"),
    }
    train_reference = {k: _brief(score(*v, SELECTION_WINDOW, BASE_COST)) for k, v in arms.items()}
    held_full = {k: {"base": score(*v, HELD_OUT_WINDOW, BASE_COST),
                     "stress": score(*v, HELD_OUT_WINDOW, STRESS_COST)} for k, v in arms.items()}
    candidate_positions = held_full["candidate_both"]["base"]["positions_list"]
    held_out = {k: {c: _brief(s) for c, s in v.items()} for k, v in held_full.items()}
    for k, v in held_full.items():
        held_out[k]["base"]["per_dataset"] = v["base"]["per_dataset"]
    defaults_reference = _brief(score(cand_seed, CANDIDATE, defaults, "both", HELD_OUT_WINDOW, BASE_COST))

    variants = []
    for row in train_rows:
        held = score(cand_seed, CANDIDATE, row["params"], SELECTION_DIRECTION, HELD_OUT_WINDOW, BASE_COST)
        variants.append({"label": row["label"],
                         "train_mean_sharpe": row["summary"]["mean_sharpe"],
                         "train_positions": row["summary"]["positions"],
                         "train_eligible": eligible(row["summary"], expected),
                         "test_mean_sharpe": held["mean_sharpe"],
                         "test_mean_return_pct": held["mean_return_pct"],
                         "test_positions": held["positions"],
                         "test_clusters": held["clusters"]})
    stability = {
        "spearman_train_vs_test_sharpe": spearman([v["train_mean_sharpe"] for v in variants],
                                                  [v["test_mean_sharpe"] for v in variants]),
        "test_net_positive_variants": sum(1 for v in variants if v["test_mean_return_pct"] > 0),
        "variants": len(variants),
        "selected_plateau": neighbors(selected, train_rows) if selection["selected"] else None,
    }
    evidence = {
        "provenance_kind": manifest["provenance"]["kind"],
        "proxy_only": manifest["provenance"]["kind"] != "venue",
        "candle_source": manifest["provenance"]["label"],
        "volume_source": "the same Hyperliquid candleSnapshot rows (base-asset volume per bar)",
        "spread_slippage": "per-coin half spread plus flat slippage per side, both scaled by the cost multiplier",
        "lot_minimum": "manifest execution spec: szDecimals lot rounding and the venue minimum order value",
        "funding": "frozen hourly Hyperliquid funding attached by the manifest and charged while positions are open",
    }
    outcome = verdict(held_out, selection, evidence)
    return {
        "study": manifest["study"],
        "issue": ISSUE,
        "tracking_group": TRACKING_GROUP,
        "protocol": proto,
        "protocol_sha256": protocol_sha256(proto),
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
        "evidence": evidence,
        "incumbents_reference": list(INCUMBENTS),
        "train_sweep": train_rows,
        "selection": selection,
        "selected_params": selected,
        "ablation_params": ablation,
        "held_out_specs": {"held_out_candidate.json": arm_spec(cand_seed, CANDIDATE, selected, "both"),
                           "held_out_ablation.json": arm_spec(cand_seed, CANDIDATE, ablation, "both")},
        "baseline_params": baseline,
        "train_reference": train_reference,
        "held_out": held_out,
        "defaults_held_out": defaults_reference,
        "candidate_variants_held_out": variants,
        "parameter_stability": stability,
        "entry_audit": entry_audit(reg, manifest, selected, candidate_positions),
        "frozen_parity": frozen_parity(manifest, cand_seed, selected),
        "verdict": outcome,
        "verdict_guards": verdict_guards(held_out, selection, evidence),
        "close_validation": aggregate_close_validations(
            s.get("close_validation") for arm in held_out.values() for s in arm.values()),
    }


def _fmt(v, nd=2):
    if v is None:
        return "-"
    if isinstance(v, bool):
        return "yes" if v else "no"
    if isinstance(v, float):
        return f"{v:.{nd}f}"
    return str(v)


ARM_LABELS = {
    "candidate_both": "candidate, both sides",
    "ablation_both": "price-pivot ablation, both sides",
    "candidate_long": "candidate, long only",
    "ablation_long": "price-pivot ablation, long only",
    "baseline_long": "volume_weighted defaults, long only",
}


def render(result: dict) -> str:
    v = result["verdict"]
    m = result["manifest"]
    p = result["protocol"]
    seed = p["candidate_seed"]
    lines = [
        f"# On Balance Volume divergence: held-out result (#{result['issue']})",
        "",
        f"Verdict: **{v['outcome'].upper()}**. {v['reason']}.",
        "",
        f"Registry evidence source for this outcome: `{v['edge_source']}`. {v['availability']}.",
        "",
        "Generated by `run_study.py` from `results.json`; do not edit by hand.",
        "",
        "## Inputs",
        "",
        f"- Protocol sha256 `{result['protocol_sha256']}` (frozen before held-out scoring; see README)",
        f"- Manifest `{m['path']}` sha256 `{m['sha256']}`",
        f"- Provenance: {m['provenance']['kind']}: {m['provenance']['label']}",
        f"- Interval {m['interval']}, warm-up {m['warmup_bars']} bars (not scored)",
    ]
    for name, w in sorted(m["windows"].items(), key=lambda kv: kv[1]["start"]):
        lines.append(f"- Window `{name}` [{w['start']}, {w['end']}): {w['role']}")
    c = m["costs"]
    lines.append(
        f"- Costs: taker {c['taker_fee_pct']*100:.3f}%, maker {c['maker_fee_pct']*100:.3f}%, "
        f"slippage {c['slippage_bps']} bps per side plus per-coin half spread, minimum order "
        f"${c['min_notional_usd']:.0f} with a {c['min_notional_margin']*100:.0f}% margin, funding booked hourly")
    for d in m["datasets"]:
        lines.append(f"- `{d['key']}`: szDecimals {d['size_decimals']}, half spread {d['half_spread_bps']} bps, "
                     f"candles `{d['candles_sha256'][:16]}`, funding `{(d['funding_sha256'] or '')[:16]}`")
    ev = result["evidence"]
    lines += [
        f"- Candle and volume source: {ev['candle_source']}; volume: {ev['volume_source']}. Proxy-only: "
        f"{_fmt(ev['proxy_only'])}.",
        f"- Spread and slippage: {ev['spread_slippage']}. Lot and minimum: {ev['lot_minimum']}. "
        f"Funding: {ev['funding']}.",
        "",
        "## Protocol",
        "",
        f"- Close owner `tiered_tp_pct` (one tier: profit {seed['close_strategies'][0]['params']['tp_tiers'][0]['profit_pct']}, "
        f"cumulative close fraction {seed['close_strategies'][0]['params']['tp_tiers'][0]['close_fraction']}), "
        f"stop owner fixed entry ATR x{seed['stop_loss_atr_mult']}, comparison mode `{seed['comparison_mode']}`, "
        f"capital ${p['capital']:.0f}, intrabar `{p['intrabar_resolution']}`. Entries and take-profit closes "
        "decided on bar N fill at the bar N+1 open; the protective stop keeps the engine's intrabar timing.",
        f"- Selection: {p['selection_rule']}.",
        f"- Verdict rule: {p['verdict_rule']}.",
        "- Ablation: the selected parameters with `volume_test` false (price pivots, reclaim, lifetime, data "
        "validation and the zero-volume hold stay on). The baseline is unchanged `volume_weighted` defaults, "
        "long-only as registered (`short_entries=False`).",
        "",
        "## Selection (train)",
        "",
        f"- Rule outcome `{result['selection']['rule_outcome']}`; {result['selection'].get('eligible', 0)} of "
        f"{len(result['train_sweep'])} combinations eligible.",
        f"- Selected `{json.dumps(result['selected_params'], sort_keys=True)}`",
    ]
    plateau = result["parameter_stability"].get("selected_plateau") or {}
    sel = next((r for r in result["train_sweep"] if r["label"] == result["selection"].get("selected")), None)
    if sel:
        s = sel["summary"]
        lines.append(f"- Train mean Sharpe {_fmt(s['mean_sharpe'], 3)}, mean return {_fmt(s['mean_return_pct'])}%, "
                     f"positions {s['positions']}, clusters {s['clusters']}; one-step neighbours: "
                     f"{plateau.get('neighbor_count')} (median Sharpe {_fmt(plateau.get('neighbor_median_sharpe'), 3)}, "
                     f"min {_fmt(plateau.get('neighbor_min_sharpe'), 3)}, positive {plateau.get('neighbor_positive_sharpe')})")
    lines += ["", "## Train reference (selected parameters, cost x1)", "",
              "| Arm | Mean ret % | Mean Sharpe | Worst DD % | Positions (L/S) | Clusters |",
              "|---|---|---|---|---|---|"]
    for k in ARM_LABELS:
        s = result["train_reference"][k]
        lines.append(f"| {ARM_LABELS[k]} | {_fmt(s['mean_return_pct'])} | {_fmt(s['mean_sharpe'], 3)} | "
                     f"{_fmt(s['worst_max_dd_pct'])} | {s['positions']} ({s['long_positions']}/{s['short_positions']}) | "
                     f"{s['clusters']} |")
    lines += [
        "",
        "## Held-out comparison",
        "",
        "| Arm | Cost | Mean ret % | Mean Sharpe | Mean DDadj | Worst DD % | Net $ | Fees $ | Funding $ | Turnover | Positions (L/S) | Clusters | Top dataset share | Top-5 share | M1 incumbent verdict |",
        "|---|---|---|---|---|---|---|---|---|---|---|---|---|---|---|",
    ]
    for k in ARM_LABELS:
        arm = result["held_out"][k]
        for label, s in (("x1", arm["base"]), ("x2", arm["stress"])):
            lines.append(
                f"| {ARM_LABELS[k]} | {label} | {_fmt(s['mean_return_pct'])} | {_fmt(s['mean_sharpe'], 3)} | "
                f"{_fmt(s['mean_ddadj'], 3)} | {_fmt(s['worst_max_dd_pct'])} | {_fmt(s['net_pnl_usd'])} | "
                f"{_fmt(s['fees_usd'])} | {_fmt(s['funding_pnl_usd'])} | {_fmt(s['mean_turnover'])} | "
                f"{s['positions']} ({s['long_positions']}/{s['short_positions']}) | {s['clusters']} | "
                f"{_fmt(s['top_dataset_abs_pnl_share'])} | {_fmt(s['top5_position_abs_pnl_share'])} | "
                f"{s['incumbent_m1_verdict']} |")
    d = result["defaults_held_out"]
    lines += ["", f"Registry defaults, both sides, x1 (reference only): mean ret {_fmt(d['mean_return_pct'])}%, "
              f"Sharpe {_fmt(d['mean_sharpe'], 3)}, positions {d['positions']}, clusters {d['clusters']}."]
    lines += ["", "## Candidate per dataset (held-out, both sides, cost x1)", "",
              "| Dataset | Ret % | Sharpe | Max DD % | Net $ | Fees $ | Funding $ | Positions (L/S) | Time in market | Candle coverage | Funding coverage |",
              "|---|---|---|---|---|---|---|---|---|---|---|"]
    for ds, s in sorted(result["held_out"]["candidate_both"]["base"]["per_dataset"].items()):
        fc = s["funding_coverage"]
        lines.append(f"| {ds} | {_fmt(s['return_pct'])} | {_fmt(s['sharpe'], 3)} | {_fmt(s['max_dd_pct'])} | "
                     f"{_fmt(s['net_pnl_usd'])} | {_fmt(s['fees_usd'])} | {_fmt(s['funding_pnl_usd'])} | "
                     f"{s['positions']} ({s['long_positions']}/{s['short_positions']}) | {_fmt(s['time_in_market'])} | "
                     f"{s['candle_coverage']['present_bars']}/{s['candle_coverage']['expected_bars']} | "
                     f"{fc.get('present_hours')}/{fc.get('expected_hours')} |")
    lines += ["", "## Verdict checks", ""]
    for k, ok in sorted(v["checks"].items()):
        lines.append(f"- {k}: {_fmt(ok)}")
    g = result["verdict_guards"]
    lines += ["", "Guard fixtures (the same verdict rule on altered copies of this result): " +
              ", ".join(f"{k} -> {g[k]}" for k in sorted(g)) + "."]
    lines += ["", "## Frozen-input parity (held-out frame with warm-up)", "",
              "`unverified` is the strict-mode close label (a live-supported close with no approximation); "
              "`approximate` mode would read `incomplete`.", "",
              "| Dataset | Window | Batched | Input sha256 | Frame bars | Compared bars | Entry decisions | Mismatches | Close parity status | Clean |",
              "|---|---|---|---|---|---|---|---|---|---|"]
    for row in result["frozen_parity"]:
        lines.append(f"| {row['dataset']} | {row['window']} | {_fmt(row['batched'])} | `{row['input_sha256'][:16]}` | "
                     f"{row['frame_bars']} | {row['compared_bars']} | {row['entry_decisions']} | {row['mismatches']} | "
                     f"{row.get('close_parity') or '-'} | {_fmt(row.get('clean'))} |")
    lines += ["", "## Entry audit (held-out, candidate both sides, cost x1)", "",
              "| Dataset | Side | Signal bar | Fill bar | Exit | Net $ | Pivots | Divergence | Reclaim | Support sha256 |",
              "|---|---|---|---|---|---|---|---|---|---|"]
    for e in result["entry_audit"]:
        dg = e["diagnostics"]
        lines.append(f"| {e['dataset']} | {e['side']} | {e['signal_bar']} | {e['fill_bar']} | "
                     f"{','.join(e['exit_reasons'] or [])} | {_fmt(e['net_pnl_usd'])} | "
                     f"{_fmt(dg['pivot1_price'], 4)} -> {_fmt(dg['pivot2_price'], 4)} | "
                     f"{_fmt(dg['divergence'], 4)} | {_fmt(dg['reclaim'], 4)} | `{e['support_sha256'][:16]}` |")
    st = result["parameter_stability"]
    lines += [
        "",
        "## Parameter stability and unsuccessful variants",
        "",
        f"- Spearman rank correlation of train vs held-out mean Sharpe over all {st['variants']} candidate "
        f"variants: {_fmt(st['spearman_train_vs_test_sharpe'], 3)}",
        f"- Held-out net-positive variants: {st['test_net_positive_variants']}/{st['variants']}; every variant "
        "with a non-positive held-out return is unsuccessful. Full rows are in `results.json` "
        "(`candidate_variants_held_out`).",
        "",
        "Ten best and ten worst held-out variants by mean Sharpe:",
        "",
        "| Variant | Train Sharpe | Train eligible | Held-out Sharpe | Held-out ret % | Held-out positions |",
        "|---|---|---|---|---|---|",
    ]
    ranked = sorted(result["candidate_variants_held_out"], key=lambda r: (-r["test_mean_sharpe"], r["label"]))
    for row in ranked[:10] + ranked[-10:]:
        lines.append(f"| {row['label']} | {_fmt(row['train_mean_sharpe'], 3)} | {_fmt(row['train_eligible'])} | "
                     f"{_fmt(row['test_mean_sharpe'], 3)} | {_fmt(row['test_mean_return_pct'])} | {row['test_positions']} |")
    lines += ["", "## Source hashes", ""]
    for path, digest in sorted(result["source_sha256"].items()):
        lines.append(f"- `{path}` `{digest}`")
    lines.append("")
    return "\n".join(lines)


def main(argv=None) -> int:
    p = argparse.ArgumentParser(description="#1657 frozen held-out study driver (research only)")
    p.add_argument("--manifest", default=MANIFEST)
    p.add_argument("--json", default=os.path.join(_HERE, "results.json"), dest="json_out")
    p.add_argument("--report", default=os.path.join(_HERE, "REPORT.md"))
    p.add_argument("--render-only", action="store_true", help="Re-render the report from the saved results JSON")
    p.add_argument("--protocol-hash", action="store_true", help="Print the frozen protocol sha256 and exit")
    args = p.parse_args(argv)
    if args.protocol_hash:
        print(protocol_sha256(protocol(*load_seeds())))
        return 0
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
        for name, spec in result["held_out_specs"].items():
            with open(os.path.join(os.path.dirname(os.path.abspath(args.json_out)), name), "w") as fh:
                json.dump(spec, fh, indent=2)
                fh.write("\n")
    with open(args.report, "w") as fh:
        fh.write(render(result))
    print(f"verdict: {result['verdict']['outcome']} ({result['verdict']['reason']})")
    print(f"registry edge_source for this outcome: {result['verdict']['edge_source']}")
    print(f"wrote {args.json_out} and {args.report}")
    return 0


if __name__ == "__main__":
    sys.exit(main())

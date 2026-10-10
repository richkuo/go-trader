#!/usr/bin/env python3

import argparse
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
import regime_label_columns as rlc
from backtester import aggregate_close_validations, decode_close_validation
from eval_windows import (INCUMBENTS, evaluate_window, expand_sweep,
                          manifest_datasets)
from optimizer import DEFAULT_PARAM_RANGES
from parity_diff import ParityConfig, compute_parity_frame
from parity_diff import summarize as parity_summarize

MANIFEST = os.path.join(_HERE, "study_manifest.json")
SPEC = os.path.join(_HERE, "candidate_spec.json")
ISSUE = 1650
CANDIDATE = "williams_r_reversal"
COMPARATORS = ("rsi", "stoch_rsi", "mean_reversion_pro")
ARMS = (CANDIDATE,) + COMPARATORS
LONG_ONLY_CONTROL_DIRECTION = "long"
PARITY_WINDOWS = (None, 200)
_ORIGINAL_ATTACH = rlc.attach_regime_label_columns
_LABEL_MEMO: dict = {}


def load_spec(path: str = SPEC) -> dict:
    with open(path) as fh:
        spec = json.load(fh)
    if spec.get("candidate") != CANDIDATE or tuple(spec.get("comparators") or ()) != COMPARATORS:
        raise ValueError(f"{path}: candidate/comparators do not match the runner")
    return spec


def _regime_plan(spec: dict) -> dict:
    gate = spec["range_regime_gate"]
    return rlc.resolve_feature_windows({}, {
        "enabled": True,
        "period": int(gate["period"]),
        "adx_threshold": float(gate["adx_threshold"]),
        "timeframe": gate["timeframe"],
    })


def _frame_digest(df: pd.DataFrame) -> str:
    cols = [c for c in ("open", "high", "low", "close", "volume") if c in df.columns]
    hashed = pd.util.hash_pandas_object(df[cols], index=True).to_numpy()
    return hashlib.sha256(hashed.tobytes()).hexdigest()


def _memo_attach(df, plan, *, regime_frame=None):
    if regime_frame is not None:
        return _ORIGINAL_ATTACH(df, plan, regime_frame=regime_frame)
    key = (_frame_digest(df), json.dumps(plan, sort_keys=True, default=str))
    cached = _LABEL_MEMO.get(key)
    if cached is None:
        df, engine = _ORIGINAL_ATTACH(df, plan)
        _LABEL_MEMO[key] = {c: df[c].to_numpy(copy=True) for c in df.columns
                            if c == "regime" or str(c).startswith("regime_w_")}
        return df, engine
    for col, values in cached.items():
        df[col] = values
    return df, rlc.engine_label_columns(plan)


def _sha(path: str) -> str:
    return om.sha256_file(path)


def arm_candidate(spec: dict, plan: dict, name: str, params: dict, direction: str = "") -> dict:
    gate = spec["range_regime_gate"]
    return {
        "name": name,
        "params": dict(params),
        "direction": direction or spec["direction"],
        "close_strategies": [dict(c, params=json.loads(json.dumps(c["params"])))
                             for c in spec["close_strategies"]],
        "stop_loss_atr_mult": spec["stop_loss_atr_mult"],
        "comparison_mode": spec["comparison_mode"],
        "allowed_regimes": list(gate["allowed_labels"]),
        "regime_label_windows": plan,
        "regime_gate_on_failure": gate["on_failure"],
    }


def _grid(name: str, defaults: dict) -> list:
    ranges = DEFAULT_PARAM_RANGES[name]
    specs = [(k, list(v)) for k, v in ranges.items()]
    return expand_sweep(dict(defaults), specs)


def preflight(reg, spec: dict) -> dict:
    checked = {}
    for name in ARMS:
        defaults = dict(reg.STRATEGY_REGISTRY[name]["default_params"])
        combos = [("defaults", defaults)] + _grid(name, defaults)
        for _label, params in combos:
            reg.validate_params(name, params)
        checked[name] = len(combos)
    seed = spec["seed_params"]
    reg.validate_params(CANDIDATE, seed)
    if dict(reg.STRATEGY_REGISTRY[CANDIDATE]["default_params"]) != seed:
        raise ValueError("candidate registry defaults differ from the frozen seed")
    frozen = {k: list(v) for k, v in DEFAULT_PARAM_RANGES[CANDIDATE].items()}
    if frozen != spec["optimizer_grid"]:
        raise ValueError("candidate optimizer grid differs from the frozen specification")
    return {"validated_param_sets": checked, "seed_matches_registry": True,
            "grid_matches_spec": True}


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
        man = r["leg"]["manifest"]
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
            "trade_records": ex["trade_records"],
            "long_positions": ex["long_positions"],
            "short_positions": ex["short_positions"],
            "time_in_market": round(ex["bars_in_market"] / window_bars[r["dataset"]], 4),
            "rejected_entries": ex.get("rejected_entries", 0),
            "skipped_partial_closes": ex.get("skipped_partial_closes", 0),
            "candle_coverage_complete": man["candle_coverage"]["complete"],
            "funding_coverage_complete": man["funding_coverage"]["complete"],
            "funding_coverage": man["funding_coverage"],
            "funding_sha256": man["funding_sha256"],
            "candles_sha256": man["candles_sha256"],
            "funding_incomplete": bool(r["leg"].get("funding_incomplete")),
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
        "trade_records": sum(v["trade_records"] for v in per_dataset.values()),
        "long_positions": sum(1 for p in positions if p["side"] == "long"),
        "short_positions": sum(1 for p in positions if p["side"] == "short"),
        "liquidated_legs": sum(1 for l in legs if l.get("liquidated")),
        "top_dataset_abs_pnl_share": (round(max(abs_ds) / sum(abs_ds), 4) if sum(abs_ds) > 0 else None),
        "top5_position_abs_pnl_share": (round(sum(abs_pos[:5]) / sum(abs_pos), 4) if sum(abs_pos) > 0 else None),
        "same_side_cross_asset_overlap_share": (round(overlap / len(positions), 4) if positions else None),
        "coverage_complete": all(v["candle_coverage_complete"] and v["funding_coverage_complete"]
                                 and not v["funding_incomplete"]
                                 for v in per_dataset.values()),
        "incumbent_m1_verdict": score.get("verdict"),
        "incumbent_bar_mean_sharpe": score.get("mean_bar_sharpe"),
        "incumbent_bar_mean_ddadj": score.get("mean_bar_ddadj"),
        "per_dataset": per_dataset,
        "close_validation": score.get("close_validation"),
    }


def select(rows: list, min_positions: int) -> dict:
    eligible = [r for r in rows
                if r["summary"]["datasets"] == r["summary"].get("expected_datasets")
                and r["summary"]["coverage_complete"]
                and r["summary"]["liquidated_legs"] == 0
                and r["summary"]["positions"] >= min_positions]
    if not eligible:
        return {"rule_outcome": "no_eligible_combo", "selected": None, "eligible": 0}
    best = max(enumerate(eligible),
               key=lambda kv: (kv[1]["summary"]["mean_sharpe"],
                               kv[1]["summary"]["mean_ddadj"], -kv[0]))[1]
    return {"rule_outcome": "selected", "selected": best["label"], "params": best["params"],
            "eligible": len(eligible)}


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


def _parity_complete(validation) -> bool:
    decoded = decode_close_validation(validation)
    return decoded["decode_status"] == "ok" and not decoded["incomplete_parity"]


def verdict(spec: dict, arms: dict, test: dict) -> dict:
    rule = spec["verdict_rule"]
    cand = test[CANDIDATE]["selected"]["base"]
    stress = test[CANDIDATE]["selected"]["stress"]
    comps = {c: test[c]["selected"]["base"] for c in COMPARATORS}
    evidence = {
        "coverage_complete": (cand["coverage_complete"] and stress["coverage_complete"]
                              and all(c["coverage_complete"] for c in comps.values())),
        "train_selection_eligible": arms[CANDIDATE]["selection"]["selected"] is not None,
        "sample_sufficient": cand["positions"] >= rule["min_test_positions"],
        "close_parity_complete": all(
            _parity_complete(s["close_validation"]) for s in [cand, stress] + list(comps.values())),
    }
    performance = {
        "net_positive_base": cand["mean_return_pct"] > 0,
        "net_positive_stress": stress["mean_return_pct"] > 0,
        "beats_comparators_sharpe": all(cand["mean_sharpe"] > c["mean_sharpe"] for c in comps.values()),
        "beats_comparators_ddadj": all(cand["mean_ddadj"] > c["mean_ddadj"] for c in comps.values()),
        "beats_incumbent_bar": cand["incumbent_m1_verdict"] == "pass",
    }
    missing = sorted(k for k, v in evidence.items() if not v)
    failed = sorted(k for k, v in performance.items() if not v)
    if missing:
        outcome, reason = "inconclusive", "evidence check not met: " + ", ".join(missing)
    elif failed:
        outcome, reason = "fail", "failed frozen checks: " + ", ".join(failed)
    else:
        outcome, reason = "pass", "every frozen held-out check passed"
    source = {"pass": "unvalidated", "fail": "study_fail", "inconclusive": "study_inconclusive"}[outcome]
    return {"outcome": outcome, "reason": reason,
            "checks": {**evidence, **performance},
            "registry_edge_source": source,
            "live_status": ("edge_status stays no_edge for every outcome; paper evaluation needs an "
                            "explicit --mode=paper and live use needs allow_no_edge: true")}


def frozen_parity(spec: dict, plan: dict, manifest: dict, params: dict) -> list:
    rows = []
    for ds in manifest["datasets"]:
        frame, window_range, _ = om.window_frame(manifest, ds, spec["held_out_window"])
        scored_start = pd.Timestamp(window_range["start"])
        cfg = ParityConfig(strategy_name=CANDIDATE, params=dict(params), registry="futures",
                           platform="hyperliquid", symbol=ds["coin"], timeframe=manifest["interval"],
                           close_refs=[dict(c, params=json.loads(json.dumps(c["params"])))
                                       for c in spec["close_strategies"]],
                           direction=spec["direction"], comparison_mode=spec["comparison_mode"],
                           regime_enabled=True, regime_label_plan=plan,
                           regime_timeframe=spec["range_regime_gate"]["timeframe"])
        for window in PARITY_WINDOWS:
            parity = compute_parity_frame(frame, cfg=cfg, window=window)
            parity_summary = parity_summarize(parity)
            mismatched = parity[~parity["match"]]
            in_window = parity[pd.to_datetime(parity["ts"]) >= scored_start]
            rows.append({
                "dataset": ds["key"],
                "window": "every prefix" if window is None else window,
                "frame_bars": len(frame),
                "compared_bars": int(len(parity)),
                "mismatches": int(len(mismatched)),
                "mismatch_ts": [str(t) for t in mismatched["ts"]],
                "scored_window_bars": int(len(in_window)),
                "scored_window_mismatches": int((~in_window["match"]).sum()),
                "entry_decisions": int((parity["live_open_action"].isin(["long", "short"])).sum()),
                "decision_agreement": parity_summary["decision_agreement"],
                "close_parity": parity_summary["close_parity"],
            })
    return rows


def run(manifest_path: str, spec_path: str = SPEC) -> dict:
    from registry_loader import load_registry
    spec = load_spec(spec_path)
    plan = _regime_plan(spec)
    rlc.attach_regime_label_columns = _memo_attach
    manifest = om.load_manifest(manifest_path)
    reg = load_registry("futures")
    preflight_record = preflight(reg, spec)
    datasets = manifest_datasets(manifest)
    sel_w, test_w = spec["selection_window"], spec["held_out_window"]
    base_cost, stress_cost = spec["cost_multipliers"]["base"], spec["cost_multipliers"]["stress"]
    window_bars = {}
    for ds in manifest["datasets"]:
        for w in (sel_w, test_w):
            _, _, cov = om.window_frame(manifest, ds, w)
            window_bars.setdefault(w, {})[ds["key"]] = cov["present_bars"]
    memo: dict = {}

    def score(name, params, window, cost, direction=""):
        s = evaluate_window(reg, arm_candidate(spec, plan, name, params, direction), datasets, window,
                            spec["capital_usd"], memo, manifest=manifest, cost_multiplier=cost)
        out = summarize(s, window_bars[window])
        out["expected_datasets"] = len(datasets)
        return out

    min_train = spec["selection_rule"]["min_train_positions"]
    arms = {}
    for name in ARMS:
        defaults = dict(reg.STRATEGY_REGISTRY[name]["default_params"])
        train_rows = []
        for label, params in _grid(name, defaults):
            summary = score(name, params, sel_w, base_cost)
            summary.pop("per_dataset", None)
            summary.pop("close_validation", None)
            train_rows.append({"label": label, "params": params, "summary": summary})
        choice = select(train_rows, min_train)
        selected_params = choice["params"] if choice["selected"] else defaults
        arms[name] = {
            "defaults": defaults,
            "short_entries": bool(reg._registry.STRATEGIES[name]["short_entries"]),
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
                "base": score(name, arm["selected_params"], test_w, base_cost),
                "stress": score(name, arm["selected_params"], test_w, stress_cost),
            },
            "defaults": {
                "params": arm["defaults"],
                "base": score(name, arm["defaults"], test_w, base_cost),
            },
        }

    long_only = {}
    for name, arm in arms.items():
        long_only[name] = {
            "params": arm["selected_params"],
            "base": score(name, arm["selected_params"], test_w, base_cost, LONG_ONLY_CONTROL_DIRECTION),
        }

    variants = []
    for row in arms[CANDIDATE]["train_sweep"]:
        held = score(CANDIDATE, row["params"], test_w, base_cost)
        variants.append({"label": row["label"], "params": row["params"],
                         "train_mean_sharpe": row["summary"]["mean_sharpe"],
                         "train_positions": row["summary"]["positions"],
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
        "issue": ISSUE,
        "spec": {"path": os.path.relpath(spec_path, _REPO), "sha256": _sha(spec_path), "content": spec},
        "regime_label_plan": json.loads(json.dumps(plan, default=str)),
        "preflight": preflight_record,
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
        "source_sha256": {p: _sha(os.path.join(_REPO, p)) for p in spec["source_files"]},
        "incumbents_reference": list(INCUMBENTS),
        "arms": arms,
        "held_out": test,
        "long_only_controls": long_only,
        "candidate_variants_held_out": variants,
        "parameter_stability": stability,
        "frozen_parity": frozen_parity(spec, plan, manifest, arms[CANDIDATE]["selected_params"]),
        "verdict": verdict(spec, arms, test),
        "close_validation": aggregate_close_validations(
            s.get("close_validation") for arm_test in test.values()
            for s in (arm_test["selected"]["base"], arm_test["selected"]["stress"],
                      arm_test["defaults"]["base"])),
    }


def _fmt(v, nd=2):
    if v is None:
        return "-"
    if isinstance(v, bool):
        return "yes" if v else "no"
    if isinstance(v, float):
        return f"{v:.{nd}f}"
    return str(v)


def _arm_row(name: str, label: str, s: dict) -> str:
    return (f"| `{name}` | {label} | {_fmt(s['mean_return_pct'])} | {_fmt(s['mean_sharpe'], 3)} | "
            f"{_fmt(s['mean_ddadj'], 3)} | {_fmt(s['worst_max_dd_pct'])} | {_fmt(s['net_pnl_usd'])} | "
            f"{_fmt(s['fees_usd'])} | {_fmt(s['funding_pnl_usd'])} | {_fmt(s['mean_turnover'])} | "
            f"{s['positions']} ({s['long_positions']}/{s['short_positions']}) | "
            f"{_fmt(s['top_dataset_abs_pnl_share'])} | {_fmt(s['top5_position_abs_pnl_share'])} | "
            f"{_fmt(s['same_side_cross_asset_overlap_share'])} | {s['incumbent_m1_verdict']} |")


_ARM_HEADER = [
    "| Arm | Run | Mean ret % | Mean Sharpe | Mean DDadj | Worst DD % | Net $ | Fees $ | Funding $ | Turnover | Positions (L/S) | Top dataset share | Top-5 share | Cross-asset overlap | M1 incumbent verdict |",
    "|---|---|---|---|---|---|---|---|---|---|---|---|---|---|---|",
]


def _span(times: list) -> str:
    if not times:
        return "-"
    if len(times) == 1:
        return times[0]
    return f"{times[0]} to {times[-1]}"


def render(result: dict) -> str:
    v = result["verdict"]
    spec = result["spec"]["content"]
    gate = spec["range_regime_gate"]
    close = spec["close_strategies"][0]
    lines = [
        "# Williams %R reversal: held-out result (#1650)",
        "",
        f"Verdict: **{v['outcome'].upper()}**. {v['reason']}. {v['live_status']}.",
        f"Registry evidence source for this outcome: `{v['registry_edge_source']}`.",
        "",
        "Generated by `run_study.py` from `results.json`; do not edit by hand.",
        "",
        "## Inputs",
        "",
        f"- Specification `{result['spec']['path']}` sha256 `{result['spec']['sha256']}`",
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
        f"${c['min_notional_usd']:.0f} with a {c['min_notional_margin']*100:.0f}% margin, lot size from "
        "szDecimals, funding booked hourly from the manifest funding files")
    for d in result["manifest"]["datasets"]:
        lines.append(f"- `{d['key']}`: szDecimals {d['size_decimals']}, half spread {d['half_spread_bps']} bps, "
                     f"candles `{d['candles_sha256'][:16]}`, funding `{(d['funding_sha256'] or '')[:16]}`")
    pf = result["preflight"]
    lines += [
        f"- Preflight: every default and grid parameter set passed registry validation "
        f"({', '.join(f'{k} {n}' for k, n in sorted(pf['validated_param_sets'].items()))}); "
        "the candidate seed and grid match the frozen specification.",
        "",
        "## Protocol",
        "",
        f"- Every arm: direction `{spec['direction']}`, close owner `{close['name']}` "
        f"(`{json.dumps(close['params'], sort_keys=True)}`), stop owner fixed entry ATR "
        f"x{spec['stop_loss_atr_mult']} ({spec['atr_method']} ATR), capital ${spec['capital_usd']:.0f}, "
        "fills at the next bar open, leverage 1, no scale-in, no implicit reversal.",
        f"- Close comparison mode: `{spec['comparison_mode']}`.",
        f"- Range gate: classifier `{gate['classifier']}`, period {gate['period']}, ADX threshold "
        f"{gate['adx_threshold']}, allowed labels {gate['allowed_labels']}, failure policy "
        f"`{gate['on_failure']}`, timeframe `{gate['timeframe']}`, labels from the trailing "
        f"{result['regime_label_plan']['limit']} rows ending at each bar "
        f"(`{result['regime_label_plan']['source']}`, no shift); an entry decided on closed bar N is gated "
        "by bar N's label and fills at bar N+1 open. The gate blocks entries only; open positions keep "
        "their take-profit and stop.",
        f"- Selection: {spec['selection_rule']['text']}; scored on `{spec['selection_window']}` only.",
        f"- Held-out `{spec['held_out_window']}` scored once per arm at cost "
        f"x{spec['cost_multipliers']['base']} and x{spec['cost_multipliers']['stress']}.",
        "- `rsi` and `stoch_rsi` register `short_entries=False`. Under direction `both` the engine "
        "opens a short on their -1 signal, so their short legs are a matched mechanical control and "
        "do not show native short-entry support. The long-only controls below separate this.",
        "",
        "## Selections (train)",
        "",
        "| Arm | short_entries | Eligible combos | Selected params | Train mean Sharpe | Neighbour median Sharpe |",
        "|---|---|---|---|---|---|",
    ]
    for name in ARMS:
        arm = result["arms"][name]
        sel = next((r for r in arm["train_sweep"] if r["label"] == arm["selection"].get("selected")), None)
        plateau = arm["selected_plateau"] or {}
        lines.append(f"| `{name}` | {_fmt(arm['short_entries'])} | {arm['selection'].get('eligible')} | "
                     f"`{json.dumps(arm['selected_params'], sort_keys=True)}` | "
                     f"{_fmt(sel['summary']['mean_sharpe'] if sel else None)} | "
                     f"{_fmt(plateau.get('neighbor_median_sharpe'))} |")
    lines += ["", "## Held-out comparison (direction both)", ""] + _ARM_HEADER
    for name in ARMS:
        t = result["held_out"][name]
        for label, s in (("selected x1", t["selected"]["base"]), ("selected x2", t["selected"]["stress"]),
                         ("defaults x1", t["defaults"]["base"])):
            lines.append(_arm_row(name, label, s))
    lines += ["", "## Long-only matched controls (held-out, selected params, cost x1; not in the verdict)", ""]
    lines += _ARM_HEADER
    for name in ARMS:
        lines.append(_arm_row(name, "long only", result["long_only_controls"][name]["base"]))
    lines += ["", "## Candidate per dataset (held-out, selected, cost x1)", "",
              "| Dataset | Ret % | Sharpe | Max DD % | Net $ | Fees $ | Funding $ | Positions (L/S) | Trade records | Time in market | Rejected entries | Skipped partial closes | Funding coverage complete |",
              "|---|---|---|---|---|---|---|---|---|---|---|---|---|"]
    for ds, s in sorted(result["held_out"][CANDIDATE]["selected"]["base"]["per_dataset"].items()):
        lines.append(f"| {ds} | {_fmt(s['return_pct'])} | {_fmt(s['sharpe'], 3)} | {_fmt(s['max_dd_pct'])} | "
                     f"{_fmt(s['net_pnl_usd'])} | {_fmt(s['fees_usd'])} | {_fmt(s['funding_pnl_usd'])} | "
                     f"{s['positions']} ({s['long_positions']}/{s['short_positions']}) | {s['trade_records']} | "
                     f"{_fmt(s['time_in_market'])} | {s['rejected_entries']} | {s['skipped_partial_closes']} | "
                     f"{_fmt(s['funding_coverage_complete'])} |")
    lines += ["", "## Frozen-input parity (non-batched, held-out frame with warm-up, range-gate labels compared)", "",
              "Each row compares the backtest decision and range-gate labels with the live slot evaluator on "
              "every bar of the hash-verified held-out frame (warm-up included). A mismatch before the scored "
              "window falls in the unscored warm-up, where the backtest has no 200-row label yet and the live "
              "evaluator classifies the shorter prefix it was given; no trade is scored there.",
              "",
              "| Dataset | Window | Frame bars | Compared bars | Entry decisions | Mismatches (all) | Mismatch span | Scored-window bars | Scored-window mismatches | Close parity |",
              "|---|---|---|---|---|---|---|---|---|---|"]
    for row in result["frozen_parity"]:
        lines.append(f"| {row['dataset']} | {row['window']} | {row['frame_bars']} | {row['compared_bars']} | "
                     f"{row['entry_decisions']} | {row['mismatches']} | {_span(row['mismatch_ts'])} | "
                     f"{row['scored_window_bars']} | {row['scored_window_mismatches']} | {row['close_parity']} |")
    st = result["parameter_stability"]
    lines += ["", "## Verdict checks", ""]
    for k, ok in sorted(v["checks"].items()):
        lines.append(f"- {k}: {_fmt(ok)}")
    lines += [
        "",
        "## Parameter stability and unsuccessful variants",
        "",
        "Held-out variant rows are diagnostics only; the selection was fixed on the training window.",
        "",
        f"- Spearman rank correlation of train vs held-out mean Sharpe over all "
        f"{st['variants']} candidate variants: {_fmt(st['spearman_train_vs_test_sharpe'], 3)}",
        f"- Held-out net-positive variants: {st['test_net_positive_variants']}/{st['variants']}",
        "",
        "| Variant | Train Sharpe | Train positions | Held-out Sharpe | Held-out ret % | Held-out positions |",
        "|---|---|---|---|---|---|",
    ]
    for row in result["candidate_variants_held_out"]:
        lines.append(f"| {row['label']} | {_fmt(row['train_mean_sharpe'], 3)} | {row['train_positions']} | "
                     f"{_fmt(row['test_mean_sharpe'], 3)} | {_fmt(row['test_mean_return_pct'])} | "
                     f"{row['test_positions']} |")
    lines += [
        "",
        "## Limits",
        "",
        "- Each dataset is an independent $1,000 study; the aggregate rows are averages and sums of "
        "independent results and say nothing about shared-wallet portfolio performance.",
        "- The held-out window was also the held-out window of earlier studies on the same frozen data "
        "(#1649, #1645, #1647, #1660, #1666); it is held out only for this candidate's parameter selection.",
        "- The half spread is one pinned L2 snapshot, not a historical average.",
        "- The M1 incumbent bar runs the incumbents with their own registry defaults, no close owner and "
        "the flat legacy cost model on the same frozen candles and funding.",
        "",
        "## Source hashes",
        "",
    ]
    for path, digest in sorted(result["source_sha256"].items()):
        lines.append(f"- `{path}` `{digest}`")
    lines.append("")
    return "\n".join(lines)


def main(argv=None) -> int:
    p = argparse.ArgumentParser(description="#1650 frozen held-out study driver (research-only)")
    p.add_argument("--manifest", default=MANIFEST)
    p.add_argument("--spec", default=SPEC)
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
            result = run(args.manifest, args.spec)
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

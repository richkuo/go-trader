#!/usr/bin/env python3

import argparse
import hashlib
import json
import math
import os
import statistics
import sys

import pandas as pd

_HERE = os.path.dirname(os.path.abspath(__file__))
_BACKTEST = os.path.abspath(os.path.join(_HERE, "..", ".."))
_REPO = os.path.abspath(os.path.join(_BACKTEST, ".."))
for _p in (_BACKTEST, os.path.join(_REPO, "shared_tools"), os.path.join(_REPO, "shared_strategies", "open"), _HERE):
    if _p not in sys.path:
        sys.path.insert(0, _p)

import offline_manifest as om
from backtester import aggregate_close_validations
from eval_windows import evaluate_window, expand_sweep, manifest_datasets
from parity_diff import ParityConfig, compute_parity_frame
from parity_diff import summarize as parity_summarize
from reference import reference as sequential_reference
from registry_loader import load_registry

MANIFEST = os.path.join(_HERE, "study_manifest.json")
PROTOCOL = os.path.join(_HERE, "protocol.json")
PROTOCOL_SHA256 = "def1df13ff852c138b749993c26118ee652946ccc16e0aa0374e558405f11da1"
CANDIDATE = "inversion_fvg"
CONTROLS = {
    "noninverted_control": ("inversion_fvg_noninverted_retest_control", "noninverted_retest"),
    "inversion_close_control": ("inversion_fvg_inversion_close_control", "inversion_close"),
}
CONTROL_NAMES = {name: mode for name, mode in CONTROLS.values()}
PARAM_KEYS = ("min_fvg_atr", "max_gap_age", "retest_window", "entry_level")
SOURCE_FILES = (
    "shared_strategies/open/inversion_fvg.py",
    "shared_strategies/open/registry.py",
    "shared_strategies/close/tiered_tp_pct.py",
    "shared_tools/strategy_composition.py",
    "backtest/backtester.py",
    "backtest/eval_windows.py",
    "backtest/offline_manifest.py",
    "backtest/parity_diff.py",
    "backtest/optimizer.py",
    "backtest/candidates/inversion_fvg_2012/protocol.json",
    "backtest/candidates/inversion_fvg_2012/reference.py",
    "backtest/candidates/inversion_fvg_2012/run_study.py",
)


def _sha(path: str) -> str:
    return om.sha256_file(path)


def load_protocol(path: str = PROTOCOL, *, enforce: bool = True) -> dict:
    with open(path, "rb") as fh:
        raw = fh.read()
    digest = hashlib.sha256(raw).hexdigest()
    if enforce and digest != PROTOCOL_SHA256:
        raise SystemExit(f"protocol.json sha256 {digest} differs from the frozen PROTOCOL_SHA256 {PROTOCOL_SHA256}; "
                         "refusing to score a changed protocol")
    out = json.loads(raw)
    out["_sha256"] = digest
    return out


def control_strategy(mode):
    def evaluate(df, min_fvg_atr=0.25, max_gap_age=48, retest_window=24, entry_level=0.5):
        import inversion_fvg as ifvg
        return ifvg._evaluate(df, min_fvg_atr, max_gap_age, retest_window, entry_level, allow_short=True, mode=mode)
    return evaluate


class StudyRegistry:
    def __init__(self, base):
        import inversion_fvg as ifvg
        self._base = base
        self._registry = base._registry
        extra = {}
        for name, mode in CONTROL_NAMES.items():
            entry = dict(base.STRATEGY_REGISTRY[CANDIDATE])
            entry.update(fn=control_strategy(mode), description=f"study-local {mode} control (#2012); never registered",
                         param_validator=ifvg.invfvg_param_validator)
            extra[name] = entry
        self.STRATEGY_REGISTRY = {**base.STRATEGY_REGISTRY, **extra}

    def get_strategy(self, name):
        if name in CONTROL_NAMES:
            return self.STRATEGY_REGISTRY[name]
        return self._base.get_strategy(name)

    def apply_strategy(self, name, df, params=None):
        if name not in CONTROL_NAMES:
            return self._base.apply_strategy(name, df, params)
        merged = {**self.STRATEGY_REGISTRY[name]["default_params"], **(params or {})}
        merged = {k: v for k, v in merged.items() if k in PARAM_KEYS}
        return control_strategy(CONTROL_NAMES[name])(df, **merged)

    def validate_params(self, name, params=None, default_params=None):
        target = CANDIDATE if name in CONTROL_NAMES else name
        return self._base.validate_params(target, params, default_params)

    def list_strategies(self):
        return self._base.list_strategies()


def reference_signals(df: pd.DataFrame, params: dict, mode: str) -> list:
    rows = list(zip(*(df[col].astype(float).tolist() for col in ("open", "high", "low", "close"))))
    ts = [float(x.value // 1_000_000) for x in df.index]
    signals, _ = sequential_reference(rows, ts, params, True, mode)
    return signals


def arm_spec(proto: dict, strategy: str, params: dict, direction: str) -> dict:
    ex = proto["execution"]
    return {
        "name": CONTROLS[strategy][0] if strategy in CONTROLS else strategy,
        "params": dict(params),
        "direction": direction,
        "close_strategies": json.loads(json.dumps(ex["close_strategies"])),
        "stop_loss_atr_mult": ex["stop_loss_atr_mult"],
        "comparison_mode": ex["comparison_mode"],
    }


def matched_contract(proto: dict, direction: str) -> dict:
    ex = proto["execution"]
    return {"direction": direction, "close_strategies": json.loads(json.dumps(ex["close_strategies"])),
            "stop_loss_atr_mult": ex["stop_loss_atr_mult"], "comparison_mode": ex["comparison_mode"]}


def arm_grid(proto: dict, arm: dict) -> dict:
    strategy = arm["strategy"]
    ref = arm.get("grid")
    if ref == "candidate.grid":
        return proto["candidate"]["grid"]
    if ref in ("noninverted_control.grid", "inversion_close_control.grid"):
        return proto[ref.split(".")[0]]["grid"]
    return proto["comparators"][strategy]["grid"]


def arm_defaults(reg, proto: dict, arm: dict) -> dict:
    if arm["strategy"] == CANDIDATE or arm["strategy"] in CONTROLS:
        return dict(proto["candidate"]["seed"])
    return dict(reg.STRATEGY_REGISTRY[arm["strategy"]]["default_params"])


def clusters(positions: list) -> int:
    spans = sorted((pd.Timestamp(p["entry_date"]), pd.Timestamp(p["exit_date"])) for p in positions)
    count = 0
    end = None
    for start, stop in spans:
        if end is None or start > end:
            count += 1
            end = stop
        else:
            end = max(end, stop)
    return count


def summarize(score: dict, window_bars: dict) -> dict:
    rows = [r for r in score["rows"] if r["leg"] is not None]
    if not rows:
        return {"datasets": 0, "positions": 0, "clusters": 0, "coverage_complete": False, "liquidated_legs": 0}
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
            "long_net_usd": round(sum(p["net_pnl"] for p in ex["position_list"] if p["side"] == "long"), 6),
            "short_net_usd": round(sum(p["net_pnl"] for p in ex["position_list"] if p["side"] == "short"), 6),
            "time_in_market": round(ex["bars_in_market"] / window_bars[r["dataset"]], 4),
            "rejected_entries": ex.get("rejected_entries", 0),
            "skipped_partial_closes": ex.get("skipped_partial_closes", 0),
            "candle_coverage_complete": r["leg"]["manifest"]["candle_coverage"]["complete"],
            "funding_coverage_complete": r["leg"]["manifest"]["funding_coverage"]["complete"],
            "funding_incomplete": bool(r["leg"].get("funding_incomplete")),
        }
        for p in ex["position_list"]:
            positions.append(dict(p, dataset=r["dataset"]))
    return {
        "datasets": len(rows),
        "mean_return_pct": round(statistics.mean(l["return_pct"] for l in legs), 4),
        "mean_sharpe": round(statistics.mean(l["sharpe"] for l in legs), 4),
        "mean_ddadj": round(statistics.mean(l["ddadj"] for l in legs), 4),
        "worst_max_dd_pct": round(min(l["max_dd_pct"] for l in legs), 4),
        "net_pnl_usd": round(sum(v["net_pnl_usd"] for v in per_dataset.values()), 4),
        "fees_usd": round(sum(v["fees_usd"] for v in per_dataset.values()), 4),
        "funding_pnl_usd": round(sum(v["funding_pnl_usd"] for v in per_dataset.values()), 4),
        "long_net_usd": round(sum(v["long_net_usd"] for v in per_dataset.values()), 4),
        "short_net_usd": round(sum(v["short_net_usd"] for v in per_dataset.values()), 4),
        "mean_turnover": round(statistics.mean(v["turnover"] for v in per_dataset.values()), 4),
        "positions": len(positions),
        "long_positions": sum(1 for p in positions if p["side"] == "long"),
        "short_positions": sum(1 for p in positions if p["side"] == "short"),
        "clusters": clusters(positions),
        "rejected_entries": sum(v["rejected_entries"] for v in per_dataset.values()),
        "skipped_partial_closes": sum(v["skipped_partial_closes"] for v in per_dataset.values()),
        "liquidated_legs": sum(1 for l in legs if l.get("liquidated")),
        "coverage_complete": all(v["candle_coverage_complete"] and v["funding_coverage_complete"]
                                 and not v["funding_incomplete"] for v in per_dataset.values()),
        "incumbent_m1_verdict": score.get("verdict"),
        "incumbent_bar_mean_sharpe": score.get("mean_bar_sharpe"),
        "incumbent_bar_mean_ddadj": score.get("mean_bar_ddadj"),
        "per_dataset": per_dataset,
        "close_validation": score.get("close_validation"),
    }


def select(rows: list, expected: int, min_clusters: int) -> dict:
    eligible = [(i, r) for i, r in enumerate(rows)
                if r["summary"]["datasets"] == expected and r["summary"]["coverage_complete"]
                and r["summary"]["liquidated_legs"] == 0 and r["summary"]["clusters"] >= min_clusters]
    if not eligible:
        return {"rule_outcome": "no_eligible_combo", "selected": None, "params": None, "eligible": 0}
    best = max(eligible, key=lambda kv: (kv[1]["summary"]["mean_sharpe"], kv[1]["summary"]["mean_ddadj"], -kv[0]))[1]
    return {"rule_outcome": "selected", "selected": best["label"], "params": best["params"], "eligible": len(eligible)}


def neighbors(params: dict, grid: dict, held_by_label: dict) -> dict:
    values = []
    for label, row in held_by_label.items():
        steps = []
        for key, options in grid.items():
            if len(options) < 2:
                continue
            steps.append(abs(options.index(params[key]) - options.index(row["params"][key])))
        if sum(steps) == 1:
            values.append(row["mean_return_pct"])
    return {"neighbor_count": len(values),
            "neighbor_median_return_pct": round(statistics.median(values), 4) if values else None,
            "neighbor_min_return_pct": round(min(values), 4) if values else None}


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


def run(manifest_path: str) -> dict:
    proto = load_protocol()
    manifest = om.load_manifest(manifest_path)
    base = load_registry("futures")
    reg = StudyRegistry(base)
    datasets = manifest_datasets(manifest)
    expected = len(datasets)
    capital = proto["execution"]["capital"]
    costs = proto["execution"]["cost_multipliers"]
    windows = [proto["windows"]["selection"], proto["windows"]["held_out"]] + list(proto["windows"]["quarters"])
    window_bars = {}
    for ds in manifest["datasets"]:
        for w in windows:
            _, _, cov = om.window_frame(manifest, ds, w)
            window_bars.setdefault(w, {})[ds["key"]] = cov["present_bars"]
    memo = {}
    cache = {}

    def score(strategy, params, direction, window, cost):
        key = (strategy, direction, json.dumps(params, sort_keys=True), window, cost)
        if key not in cache:
            spec = arm_spec(proto, strategy, params, direction)
            s = evaluate_window(reg, spec, datasets, window, capital, memo, manifest=manifest,
                                cost_multiplier=cost, funding_mode=proto["execution"]["funding_mode"],
                                intrabar_resolution=proto["execution"]["intrabar_resolution"],
                                incumbent_contract=matched_contract(proto, direction))
            out = summarize(s, window_bars[window])
            out["expected_datasets"] = expected
            cache[key] = out
        return cache[key]

    min_train = proto["selection"]["min_train_clusters"]
    arms = {}
    for arm_name, arm in proto["arms"].items():
        grid = arm_grid(proto, arm)
        defaults = arm_defaults(base, proto, arm)
        rows = []
        for label, params in expand_sweep(defaults, [(k, list(v)) for k, v in grid.items()]):
            summary = dict(score(arm["strategy"], params, arm["direction"], "train", costs["base"]))
            summary.pop("per_dataset", None)
            rows.append({"label": label, "params": params, "summary": summary})
        choice = select(rows, expected, min_train)
        arms[arm_name] = {"strategy": arm["strategy"], "direction": arm["direction"], "defaults": defaults,
                          "grid": grid, "train_sweep": rows, "selection": choice}
        print(f"train {arm_name}: {len(rows)} combos, {choice['rule_outcome']} {choice['selected']}", file=sys.stderr)

    held_window = proto["windows"]["held_out"]
    held = {}
    for arm_name, arm in arms.items():
        choice = arm["selection"]
        if not choice["selected"]:
            held[arm_name] = None
            continue
        params = choice["params"]
        held[arm_name] = {
            "params": params,
            "base": score(arm["strategy"], params, arm["direction"], held_window, costs["base"]),
            "stress": score(arm["strategy"], params, arm["direction"], held_window, costs["stress"]),
            "quarters": {q: score(arm["strategy"], params, arm["direction"], q, costs["base"])
                         for q in proto["windows"]["quarters"]},
        }
        dds = [q["worst_max_dd_pct"] for q in held[arm_name]["quarters"].values() if q["datasets"]]
        held[arm_name]["worst_quarter_dd_pct"] = min(dds) if dds else None
        print(f"held-out {arm_name}: {held[arm_name]['base']['mean_return_pct']}", file=sys.stderr)

    seed = proto["candidate"]["seed"]
    seed_held = {"params": seed, "base": score(CANDIDATE, seed, "both", held_window, costs["base"])}

    primary = proto["verdict"]["primary_arm"]
    variants = []
    held_by_label = {}
    for row in arms[primary]["train_sweep"]:
        res = score(CANDIDATE, row["params"], arms[primary]["direction"], held_window, costs["base"])
        item = {"label": row["label"], "params": row["params"], "train_mean_sharpe": row["summary"]["mean_sharpe"],
                "train_mean_return_pct": row["summary"]["mean_return_pct"], "train_clusters": row["summary"]["clusters"],
                "test_mean_sharpe": res["mean_sharpe"], "test_mean_return_pct": res["mean_return_pct"],
                "test_positions": res["positions"], "test_clusters": res["clusters"]}
        variants.append(item)
        held_by_label[row["label"]] = {"params": row["params"], "mean_return_pct": res["mean_return_pct"]}
    stability = {
        "spearman_train_vs_test_sharpe": spearman([v["train_mean_sharpe"] for v in variants],
                                                  [v["test_mean_sharpe"] for v in variants]),
        "test_net_positive_variants": sum(1 for v in variants if v["test_mean_return_pct"] > 0),
        "variants": len(variants),
    }
    plateau = (neighbors(arms[primary]["selection"]["params"], arms[primary]["grid"], held_by_label)
               if arms[primary]["selection"]["selected"] else None)

    parity = run_parity_gates(proto, manifest, reg, arms)
    result = {
        "study": manifest["study"],
        "issue": proto["issue"],
        "protocol_sha256": proto["_sha256"],
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
                          "half_spread_bps": d["half_spread_bps"], "candles_sha256": d["candles"]["sha256"],
                          "funding_sha256": d["funding"]["sha256"] if d["funding"] else None}
                         for d in manifest["datasets"]],
            "venue_reference": {
                "meta_sha256": manifest["venue_reference"]["meta"]["sha256"],
                "l2_snapshot_sha256": manifest["venue_reference"].get("l2_snapshot", {}).get("sha256"),
                "documents": manifest["venue_reference"]["documents"],
            },
        },
        "source_sha256": {p: _sha(os.path.join(_REPO, p)) for p in SOURCE_FILES},
        "protocol": {k: v for k, v in proto.items() if k != "_sha256"},
        "arms": arms,
        "held_out": held,
        "candidate_seed_held_out": seed_held,
        "candidate_variants_held_out": variants,
        "parameter_stability": stability,
        "selected_plateau": plateau,
        "parity": parity,
    }
    result["verdict"] = verdict(proto, result)
    result["close_validation"] = aggregate_close_validations(
        s.get("close_validation") for arm in held.values() if arm
        for s in (arm["base"], arm["stress"]))
    return result


def run_parity_gates(proto: dict, manifest: dict, reg, arms: dict) -> dict:
    out = {"reference": [], "window_200": []}
    checks = (("candidate_both", CANDIDATE, "retest"),
              ("noninverted_both", CONTROLS["noninverted_control"][0], "noninverted_retest"),
              ("inversion_close_both", CONTROLS["inversion_close_control"][0], "inversion_close"))
    for arm_name, name, mode in checks:
        params = arms[arm_name]["selection"]["params"]
        if not params:
            out["reference"].append({"arm": arm_name, "status": "no_selection"})
            continue
        for ds in manifest["datasets"]:
            for window in (proto["windows"]["selection"], proto["windows"]["held_out"]):
                frame, _, _ = om.window_frame(manifest, ds, window)
                got = reg.apply_strategy(name, frame, dict(params))["signal"].astype(int).tolist()
                want = reference_signals(frame, params, mode)
                mismatches = sum(1 for a, b in zip(got, want) if a != b) + abs(len(got) - len(want))
                out["reference"].append({"arm": arm_name, "dataset": ds["key"], "window": window, "bars": len(got),
                                         "entries": sum(1 for x in got if x), "mismatches": mismatches})
    params = arms["candidate_both"]["selection"]["params"]
    if params:
        ex = proto["execution"]
        for ds in manifest["datasets"]:
            frame, _, _ = om.window_frame(manifest, ds, proto["windows"]["held_out"])
            cfg = ParityConfig(strategy_name=CANDIDATE, params=dict(params), registry="futures", platform="hyperliquid",
                               symbol=ds["coin"], timeframe=manifest["interval"],
                               close_refs=json.loads(json.dumps(ex["close_strategies"])), direction="both",
                               stop_kwargs={"stop_loss_atr_mult": ex["stop_loss_atr_mult"]},
                               comparison_mode=ex["comparison_mode"])
            frame_out = compute_parity_frame(frame[["open", "high", "low", "close", "volume"]], cfg=cfg, window=200)
            s = parity_summarize(frame_out)
            out["window_200"].append({"dataset": ds["key"], "bars_compared": s["bars_compared"],
                                      "mismatches": s["mismatches"], "clean": s["clean"],
                                      "close_parity": s.get("close_parity"),
                                      "live_entries": int((frame_out["live_open_action"] != "none").sum())})
    out["passed"] = (bool(out["reference"]) and all(r.get("mismatches") == 0 for r in out["reference"])
                     and bool(out["window_200"]) and all(r["mismatches"] == 0 for r in out["window_200"]))
    return out


def verdict(proto: dict, result: dict) -> dict:
    held = result["held_out"]
    contracts = proto["matched_contracts"]
    involved = sorted({a for c in contracts.values() for a in [c["candidate"]] + c["controls"] + c["comparators"]})
    primary = proto["verdict"]["primary_arm"]
    missing = [a for a in involved if held.get(a) is None]
    checks = {}
    if missing:
        return {"outcome": "inconclusive", "reason": "no eligible train selection for " + ", ".join(missing),
                "checks": checks, "admission": proto["verdict"]["admission"]}
    coverage = all(held[a][k]["coverage_complete"] for a in involved for k in ("base", "stress"))
    checks["coverage_complete"] = coverage
    checks["parity_gates_passed"] = result["parity"]["passed"]
    clusters_ok = held[primary]["base"]["clusters"] >= proto["verdict"]["min_test_clusters"]
    checks["sample_sufficient"] = clusters_ok
    cand = held[primary]
    checks["net_positive_base"] = cand["base"]["mean_return_pct"] > 0
    checks["net_positive_stress"] = cand["stress"]["mean_return_pct"] > 0
    for cname, c in sorted(contracts.items()):
        own = held[c["candidate"]]
        ret = own["base"]["mean_return_pct"]
        worst = own["worst_quarter_dd_pct"]
        for kind in ("controls", "comparators"):
            for other in c[kind]:
                checks[f"{cname}_beats_{other}"] = ret > held[other]["base"]["mean_return_pct"]
                theirs = held[other]["worst_quarter_dd_pct"]
                checks[f"{cname}_worst_quarter_dd_no_worse_than_{other}"] = (
                    worst is not None and theirs is not None and worst >= theirs)
    if not coverage:
        outcome, reason = "inconclusive", "candle or funding coverage incomplete on a held-out arm"
    elif not checks["parity_gates_passed"]:
        outcome, reason = "inconclusive", "a parity gate failed"
    elif not clusters_ok:
        outcome, reason = "inconclusive", (f"{cand['base']['clusters']} independent held-out clusters < "
                                           f"{proto['verdict']['min_test_clusters']}")
    else:
        perf = {k: v for k, v in checks.items()
                if k not in ("coverage_complete", "parity_gates_passed", "sample_sufficient")}
        failed = sorted(k for k, v in perf.items() if not v)
        if failed:
            outcome, reason = "fail", "failed frozen checks: " + ", ".join(failed)
        else:
            outcome, reason = "pass", "every frozen held-out check passed"
    return {"outcome": outcome, "reason": reason, "checks": checks, "admission": proto["verdict"]["admission"]}


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
    p = result["protocol"]
    ex = p["execution"]
    lines = [
        "# Inversion fair value gap retest: held-out result (#2012)",
        "",
        f"Verdict: **{v['outcome'].upper()}**. {v['reason']}.",
        "",
        f"Admission: {v['admission']}.",
        "",
        "Generated by `run_study.py` from `results.json`; do not edit by hand. The frozen specification is "
        "[`README.md`](README.md) and [`protocol.json`](protocol.json).",
        "",
        "## Inputs",
        "",
        f"- Protocol `protocol.json` sha256 `{result['protocol_sha256']}`",
        f"- Manifest `{result['manifest']['path']}` sha256 `{result['manifest']['sha256']}`",
        f"- Provenance: {result['manifest']['provenance']['kind']}: {result['manifest']['provenance']['label']}",
        f"- Prior use: {p['data']['prior_use']}",
        f"- Interval {result['manifest']['interval']}, warm-up {result['manifest']['warmup_bars']} bars (not scored)",
    ]
    for name, w in sorted(result["manifest"]["windows"].items(), key=lambda kv: (kv[1]["start"], kv[0])):
        lines.append(f"- Window `{name}` [{w['start']}, {w['end']}): {w['role']}")
    c = result["manifest"]["costs"]
    lines.append(
        f"- Costs: taker {c['taker_fee_pct']*100:.3f}%, maker {c['maker_fee_pct']*100:.3f}%, slippage "
        f"{c['slippage_bps']} bps per side plus per-coin half spread, lot rounding, minimum order "
        f"${c['min_notional_usd']:.0f} with a {c['min_notional_margin']*100:.0f}% margin, funding charged hourly; "
        f"stress x{ex['cost_multipliers']['stress']} doubles spread and slippage, fees unchanged")
    for d in result["manifest"]["datasets"]:
        lines.append(f"- `{d['key']}`: szDecimals {d['size_decimals']}, half spread {d['half_spread_bps']} bps, "
                     f"candles `{d['candles_sha256'][:16]}`, funding `{(d['funding_sha256'] or '')[:16]}`")
    lines += [
        "",
        "## Protocol",
        "",
        f"- Every arm: close owner `{ex['close_strategies'][0]['name']}` "
        f"({json.dumps(ex['close_strategies'][0]['params'], sort_keys=True)}), stop owner fixed entry ATR "
        f"x{ex['stop_loss_atr_mult']}, comparison mode `{ex['comparison_mode']}`, `{ex['intrabar_resolution']}`, "
        f"capital ${ex['capital']:.0f}, {ex['fills']}.",
        f"- Selection on `{p['windows']['selection']}`: {p['selection']['eligibility']} "
        f"(min {p['selection']['min_train_clusters']}); {p['selection']['rule']}.",
        f"- Independent positions: {p['independent_positions']}.",
        f"- Timeframe scope: {p['data']['timeframe_scope']}.",
        f"- Non-inverted retest control: {p['noninverted_control']['definition']}.",
        f"- Inversion-close control: {p['inversion_close_control']['definition']}.",
        "",
        "## Comparator mappings",
        "",
    ]
    for name, comp in sorted(p["comparators"].items()):
        lines.append(f"- `{name}` (short entries: {_fmt(comp['short_entries'])}): {comp['mapping']}")
    for name, why in sorted(p["excluded_comparators"].items()):
        lines.append(f"- `{name}` excluded: {why}")
    lines += [
        "",
        "## Selections (train)",
        "",
        "| Arm | Direction | Combos | Eligible | Selected params | Train mean Sharpe | Train clusters |",
        "|---|---|---|---|---|---|---|",
    ]
    for name, arm in result["arms"].items():
        sel = arm["selection"]
        row = next((r for r in arm["train_sweep"] if r["label"] == sel.get("selected")), None)
        lines.append(f"| `{name}` | {arm['direction']} | {len(arm['train_sweep'])} | {sel.get('eligible', 0)} | "
                     f"`{json.dumps(sel.get('params'), sort_keys=True)}` | "
                     f"{_fmt(row['summary']['mean_sharpe'] if row else None, 3)} | "
                     f"{row['summary']['clusters'] if row else '-'} |")
    lines += [
        "",
        "## Held-out comparison",
        "",
        "| Arm | Cost | Mean ret % | Mean Sharpe | Mean DDadj | Worst DD % | Worst quarter DD % | Net $ | Long $ | "
        "Short $ | Fees $ | Funding $ | Turnover | Positions (L/S) | Clusters | Rejected | Skipped partial | "
        "M1 incumbent verdict |",
        "|---|---|---|---|---|---|---|---|---|---|---|---|---|---|---|---|---|---|",
    ]
    for name, h in result["held_out"].items():
        if h is None:
            lines.append(f"| `{name}` | - | no eligible selection |" + " |" * 15)
            continue
        for label, s in (("x1", h["base"]), ("x2", h["stress"])):
            lines.append(
                f"| `{name}` | {label} | {_fmt(s['mean_return_pct'])} | {_fmt(s['mean_sharpe'], 3)} | "
                f"{_fmt(s['mean_ddadj'], 3)} | {_fmt(s['worst_max_dd_pct'])} | "
                f"{_fmt(h['worst_quarter_dd_pct']) if label == 'x1' else '-'} | {_fmt(s['net_pnl_usd'])} | "
                f"{_fmt(s['long_net_usd'])} | {_fmt(s['short_net_usd'])} | {_fmt(s['fees_usd'])} | "
                f"{_fmt(s['funding_pnl_usd'])} | {_fmt(s['mean_turnover'])} | "
                f"{s['positions']} ({s['long_positions']}/{s['short_positions']}) | {s['clusters']} | "
                f"{s['rejected_entries']} | {s['skipped_partial_closes']} | {s['incumbent_m1_verdict']} |")
    seed = result["candidate_seed_held_out"]["base"]
    lines += ["", f"Candidate seed `{json.dumps(result['candidate_seed_held_out']['params'], sort_keys=True)}` "
                  f"held-out (both, x1, descriptive): mean return {_fmt(seed['mean_return_pct'])}%, mean Sharpe "
                  f"{_fmt(seed['mean_sharpe'], 3)}, {seed['positions']} positions, {seed['clusters']} clusters."]
    lines += ["", "## Held-out quarters (selected, cost x1, mean return % / worst DD %)", "",
              "| Arm | " + " | ".join(p["windows"]["quarters"]) + " |",
              "|---|" + "---|" * len(p["windows"]["quarters"])]
    for name, h in result["held_out"].items():
        if h is None:
            continue
        cells = [f"{_fmt(h['quarters'][q]['mean_return_pct'])} / {_fmt(h['quarters'][q]['worst_max_dd_pct'])}"
                 for q in p["windows"]["quarters"]]
        lines.append(f"| `{name}` | " + " | ".join(cells) + " |")
    primary = p["verdict"]["primary_arm"]
    if result["held_out"].get(primary):
        lines += ["", f"## `{primary}` per dataset (held-out, selected, cost x1)", "",
                  "| Dataset | Ret % | Sharpe | Max DD % | Net $ | Long $ | Short $ | Fees $ | Funding $ | "
                  "Positions (L/S) | Time in market | Rejected entries | Skipped partial closes |",
                  "|---|---|---|---|---|---|---|---|---|---|---|---|---|"]
        for ds, s in sorted(result["held_out"][primary]["base"]["per_dataset"].items()):
            lines.append(f"| {ds} | {_fmt(s['return_pct'])} | {_fmt(s['sharpe'], 3)} | {_fmt(s['max_dd_pct'])} | "
                         f"{_fmt(s['net_pnl_usd'])} | {_fmt(s['long_net_usd'])} | {_fmt(s['short_net_usd'])} | "
                         f"{_fmt(s['fees_usd'])} | {_fmt(s['funding_pnl_usd'])} | "
                         f"{s['positions']} ({s['long_positions']}/{s['short_positions']}) | "
                         f"{_fmt(s['time_in_market'])} | {s['rejected_entries']} | {s['skipped_partial_closes']} |")
    lines += ["", "## Parity gates", ""]
    for r in result["parity"]["reference"]:
        if r.get("status"):
            lines.append(f"- reference `{r['arm']}`: {r['status']}")
        else:
            lines.append(f"- reference `{r['arm']}` {r['dataset']} `{r['window']}`: {r['bars']} bars, "
                         f"{r['entries']} entries, {r['mismatches']} mismatches")
    for r in result["parity"]["window_200"]:
        lines.append(f"- 200-bar window `candidate_both` {r['dataset']}: {r['bars_compared']} bars compared, "
                     f"{r['mismatches']} mismatches, {r['live_entries']} live entries, close parity "
                     f"`{r['close_parity']}`")
    lines.append(f"- passed: {_fmt(result['parity']['passed'])}")
    lines += ["", "## Verdict checks", ""]
    for k, ok in sorted(v["checks"].items()):
        lines.append(f"- {k}: {_fmt(ok)}")
    st = result["parameter_stability"]
    pl = result["selected_plateau"] or {}
    lines += [
        "",
        "## Parameter stability and unsuccessful variants",
        "",
        f"Descriptive only: {p['verdict']['descriptive_only']}.",
        "",
        f"- Selected `{primary}` one-step neighbours: {pl.get('neighbor_count', 0)}, held-out median mean return "
        f"{_fmt(pl.get('neighbor_median_return_pct'))}%, minimum {_fmt(pl.get('neighbor_min_return_pct'))}%",
        f"- Spearman rank correlation of train vs held-out mean Sharpe over all {st['variants']} `{primary}` "
        f"variants: {_fmt(st['spearman_train_vs_test_sharpe'], 3)}",
        f"- Held-out net-positive variants: {st['test_net_positive_variants']}/{st['variants']}",
        "",
        "| Variant | Train Sharpe | Train ret % | Train clusters | Held-out Sharpe | Held-out ret % | "
        "Held-out positions | Held-out clusters |",
        "|---|---|---|---|---|---|---|---|",
    ]
    for row in result["candidate_variants_held_out"]:
        lines.append(f"| {row['label']} | {_fmt(row['train_mean_sharpe'], 3)} | {_fmt(row['train_mean_return_pct'])} | "
                     f"{row['train_clusters']} | {_fmt(row['test_mean_sharpe'], 3)} | "
                     f"{_fmt(row['test_mean_return_pct'])} | {row['test_positions']} | {row['test_clusters']} |")
    lines += ["", "## Source hashes", ""]
    for path, digest in sorted(result["source_sha256"].items()):
        lines.append(f"- `{path}` `{digest}`")
    lines.append("")
    return "\n".join(lines)


def main(argv=None) -> int:
    p = argparse.ArgumentParser(description="#2012 frozen held-out study driver (research-only, suggest-only)")
    p.add_argument("--manifest", default=MANIFEST)
    p.add_argument("--json", default=os.path.join(_HERE, "results.json"), dest="json_out")
    p.add_argument("--report", default=os.path.join(_HERE, "REPORT.md"))
    p.add_argument("--render-only", action="store_true", help="Re-render the report from the committed results JSON")
    p.add_argument("--check-protocol", action="store_true", help="Verify the frozen protocol hash and exit")
    args = p.parse_args(argv)
    if args.check_protocol:
        print(f"protocol sha256 {load_protocol()['_sha256']} matches the frozen value")
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
    with open(args.report, "w") as fh:
        fh.write(render(result))
    print(f"verdict: {result['verdict']['outcome']} ({result['verdict']['reason']})")
    print(f"wrote {args.json_out} and {args.report}")
    return 0


if __name__ == "__main__":
    sys.exit(main())

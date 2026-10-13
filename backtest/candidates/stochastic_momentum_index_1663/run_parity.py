#!/usr/bin/env python3

import argparse
import hashlib
import json
import os
import sys

import numpy as np
import pandas as pd

_HERE = os.path.dirname(os.path.abspath(__file__))
_BACKTEST = os.path.abspath(os.path.join(_HERE, "..", ".."))
_REPO = os.path.abspath(os.path.join(_BACKTEST, ".."))
for _p in (_BACKTEST, os.path.join(_REPO, "shared_tools")):
    if _p not in sys.path:
        sys.path.insert(0, _p)

import offline_manifest as om
from closed_bar import TIMESTAMP_KIND_OPEN, declare_frame_cadence
from eval_windows import expand_sweep
from optimizer import DEFAULT_PARAM_RANGES
from parity_diff import ParityConfig, compute_parity_frame, summarize as parity_summarize
from registry_loader import load_registry

SEED_PATH = os.path.join(_HERE, "candidate_seed.json")
RESULTS_PATH = os.path.join(_HERE, "results.json")
PARITY_PATH = os.path.join(_HERE, "parity_results.json")
MANIFEST = os.path.join(_REPO, "backtest", "candidates", "stochastic_momentum_index_1663_manifest.json")
TAIL_BARS = 700
LIVE_WINDOW = 200
GRID_ENDS = (450, 600, 700)
DECISION_COLUMNS = ("smi_value", "smi_prev_value", "smi_signal_line", "smi_prev_signal_line", "smi_diff",
                    "smi_prev_diff", "smi_valid", "smi_reason_code", "signal")
INPUT_COLUMNS = ["open", "high", "low", "close", "volume"]

with open(SEED_PATH) as _fh:
    SEED = json.load(_fh)
NAME = SEED["name"]
DEFAULTS = dict(SEED["params"])
DOMAIN_ENDPOINTS = [
    dict(DEFAULTS, range_period=1, smoothing_period=1, signal_period=1, recursion_length=8, oversold=-99.5,
         overbought=99.5),
    dict(DEFAULTS, range_period=190, smoothing_period=1, signal_period=1, recursion_length=8),
    dict(DEFAULTS, range_period=6, smoothing_period=24, signal_period=24, recursion_length=192),
    dict(DEFAULTS, range_period=14, smoothing_period=4, signal_period=2, recursion_length=32),
    dict(DEFAULTS, range_period=100, smoothing_period=12, signal_period=12, recursion_length=98),
]


def frozen_tail(manifest: dict, key: str, bars: int = TAIL_BARS) -> pd.DataFrame:
    candles = om.load_candles(om.dataset_by_key(manifest, key))
    frame = candles[INPUT_COLUMNS].iloc[-bars:].copy()
    return declare_frame_cadence(frame, manifest["interval"], TIMESTAMP_KIND_OPEN)


def need(params: dict) -> int:
    return int(params["recursion_length"]) + int(params["range_period"]) + 1


def _same(a: pd.Series, b: pd.Series) -> bool:
    for col in DECISION_COLUMNS:
        x, y = a[col], b[col]
        if isinstance(x, float) and isinstance(y, float) and np.isnan(x) and np.isnan(y):
            continue
        if x != y:
            return False
    return True


def decision_consistency(reg, frames: dict, param_sets: list, every_prefix: bool) -> dict:
    compared = mismatched = entries = 0
    examples = []
    for key, df in frames.items():
        for label, params in param_sets:
            full = reg.apply_strategy(NAME, df, params)
            ends = range(need(params), len(df) + 1) if every_prefix else [e for e in GRID_ENDS if e <= len(df)]
            for end in ends:
                want = full[list(DECISION_COLUMNS)].iloc[end - 1]
                entries += int(want["signal"] != 0)
                for part_name, part in (("prefix", df.iloc[:end]), ("bounded_200", df.iloc[max(0, end - LIVE_WINDOW):end]),
                                        ("exact_span", df.iloc[end - need(params):end])):
                    got = reg.apply_strategy(NAME, part, params)[list(DECISION_COLUMNS)].iloc[-1]
                    compared += 1
                    if not _same(got, want):
                        mismatched += 1
                        if len(examples) < 20:
                            examples.append({"dataset": key, "params": label, "end": end, "part": part_name})
    return {"comparisons": compared, "mismatches": mismatched, "entry_endpoints": entries, "examples": examples,
            "clean": mismatched == 0 and compared > 0}


def composer_parity(frames: dict, params: dict, close_refs) -> list:
    runs = []
    for key, df in frames.items():
        tail = df.iloc[-600:].copy()
        tail.attrs = dict(df.attrs)
        for window in (LIVE_WINDOW, None):
            for batched in (False, True):
                cfg = ParityConfig(strategy_name=NAME, params=dict(params), registry="futures", platform="hyperliquid",
                                   symbol=key.split()[0], timeframe="4h",
                                   close_refs=None if close_refs is None else json.loads(json.dumps(close_refs)),
                                   direction="both", batched=batched)
                pf = compute_parity_frame(tail, cfg=cfg, window=window)
                summary = parity_summarize(pf)
                summary.pop("close_validation", None)
                runs.append({
                    "dataset": key, "window": window, "mode": "batched_paper" if batched else "composer",
                    "close_refs": close_refs is not None, "first_bar": str(tail.index[0]), "last_bar": str(tail.index[-1]),
                    "long_opens": int((pf["live_open_action"] == "long").sum()),
                    "short_opens": int((pf["live_open_action"] == "short").sum()),
                    "closing_bars": int((pf["live_close_fraction"] > 0).sum()) if "live_close_fraction" in pf else 0,
                    **summary,
                })
    return runs


def main(argv=None) -> int:
    p = argparse.ArgumentParser(description="#1663 frozen-input decision parity (offline)")
    p.add_argument("--out", default=PARITY_PATH)
    args = p.parse_args(argv)
    reg = load_registry("futures")
    manifest = om.load_manifest(MANIFEST)
    frames = {d["key"]: frozen_tail(manifest, d["key"]) for d in manifest["datasets"]}
    selected = {}
    if os.path.exists(RESULTS_PATH):
        with open(RESULTS_PATH) as fh:
            selected = {k: v for k, v in (json.load(fh).get("selections") or {}).items()
                        if k in SEED["candidate_arms"] and v}
    named = [("defaults", DEFAULTS)] + [(f"selected_{k}", v) for k, v in sorted(selected.items())]
    named += [(f"domain_{i}", d) for i, d in enumerate(DOMAIN_ENDPOINTS)]
    for _, params in named:
        reg._registry.validate_params(NAME, params)
    specs = [(k, list(v)) for k, v in DEFAULT_PARAM_RANGES[NAME].items()]
    grid = [(label, params) for label, params in expand_sweep(dict(DEFAULTS), specs)]
    sections = {}
    every = decision_consistency(reg, frames, named, every_prefix=True)
    every["summary"] = (f"defaults, selected and domain-endpoint parameters on every prefix of the last {TAIL_BARS} "
                        f"bars of each venue dataset: {every['comparisons']} full-series vs prefix, 200-row and "
                        f"exact-span comparisons, {every['mismatches']} mismatches, {every['entry_endpoints']} entry "
                        "endpoints")
    sections["every_prefix"] = every
    sampled = decision_consistency(reg, {"ETH 4h": frames["ETH 4h"]}, grid, every_prefix=False)
    sampled["summary"] = (f"all {len(grid)} grid combinations at ends {list(GRID_ENDS)} of the ETH 4h tail: "
                          f"{sampled['comparisons']} comparisons, {sampled['mismatches']} mismatches")
    sections["grid_sampled"] = sampled
    evidence_params = selected.get("smi_both") or DEFAULTS
    runs = composer_parity(frames, evidence_params, SEED["close_strategies"])
    runs += composer_parity({"BTC 4h": frames["BTC 4h"]}, DOMAIN_ENDPOINTS[3], None)
    clean_runs = all(r["clean"] and r.get("batch_clean", True) for r in runs)
    sections["composer_and_batched_paper"] = {
        "params": evidence_params, "close_strategies": SEED["close_strategies"], "runs": runs, "clean": clean_runs,
        "summary": (f"{len(runs)} compute_parity_frame runs (composer and explicit-paper solo/batch slots, 200-row "
                    f"window and full history, both directions with the frozen close owner, plus an entry-only domain "
                    f"endpoint): {sum(r['bars_compared'] for r in runs)} bars compared, "
                    f"{sum(r['mismatches'] for r in runs)} mismatches, "
                    f"{sum(r['long_opens'] for r in runs)} long and {sum(r['short_opens'] for r in runs)} short opens, "
                    f"clean {clean_runs}"),
    }
    out = {
        "study": "stochastic_momentum_index_1663",
        "manifest_sha256": om.sha256_file(MANIFEST),
        "seed_sha256": om.sha256_file(SEED_PATH),
        "source_sha256": {path: om.sha256_file(os.path.join(_REPO, path)) for path in (
            "shared_strategies/open/stochastic_momentum_index.py", "backtest/parity_diff.py",
            "shared_scripts/check_hyperliquid.py", "backtest/candidates/stochastic_momentum_index_1663/run_parity.py")},
        "inputs": {k: hashlib.sha256(df.to_csv().encode("utf-8")).hexdigest() for k, df in frames.items()},
        "sections": sections,
        "clean": all(s["clean"] for s in sections.values()),
    }
    with open(args.out, "w") as fh:
        json.dump(out, fh, indent=1, sort_keys=True, default=str)
        fh.write("\n")
    for key, block in sorted(sections.items()):
        print(f"{key}: {block['summary']}")
    print(f"clean: {out['clean']}; wrote {args.out}")
    return 0 if out["clean"] else 1


if __name__ == "__main__":
    sys.exit(main())

#!/usr/bin/env python3
"""Offline, suggest-only calibration of the ADX and composite regime labels (#1940).

The runner hashes the pre-registered protocol before it scores any setting.
Live defaults, config, and certification files are not inputs it writes.
"""
from __future__ import annotations

import argparse
import ctypes
import hashlib
import json
import math
import os
import sqlite3
import subprocess
import sys
import tempfile
from pathlib import Path

_THIS = Path(__file__).resolve()
_BACKTEST = _THIS.parents[1]
_ROOT = _THIS.parents[2]
for _p in (str(_BACKTEST), str(_ROOT), str(_ROOT / "shared_tools")):
    if _p not in sys.path:
        sys.path.insert(0, _p)

import numpy as np
import pandas as pd

from eval_windows import DATASETS, PLATFORM, WINDOWS
from regime import (
    CLASSIFIER_COMPOSITE,
    COMPOSITE_ADX_PERIOD_CAP,
    VALID_LABELS_ADX,
    VALID_LABELS_COMPOSITE,
    _composite_ready_need,
    compute_regime,
    compute_regime_composite,
    composite_feature_matrix,
    map_composite_label,
    required_regime_bars,
)
from regime_calibrate import SIGNIFICANCE_ALPHA, gate_verdict
from regime_diagnostics import forward_realized_vol, separation, stability
from regime_stats import benjamini_hochberg, rank_data

PROTOCOL_PATH = _ROOT / "docs" / "research" / "1940-artifacts" / "protocol.json"
RESULTS_PATH = _ROOT / "docs" / "research" / "1940-artifacts" / "results.json"
REPORT_PATH = _ROOT / "docs" / "research" / "1940-regime-classifier-calibration.md"
ADX_LABELS = tuple(sorted(VALID_LABELS_ADX))
COMPOSITE_LABELS = tuple(sorted(VALID_LABELS_COMPOSITE))
_KERNEL = None


def min_n_perm_for_alpha(alpha: float) -> int:
    """Same floor as regime_1080_unsupervised_vol_model.min_n_perm_for_alpha."""
    return max(1, math.ceil(1.0 / float(alpha)) - 1)


def permutation_requirement(family_size: int, alpha: float) -> tuple[int, int]:
    corrected = float(alpha) / float(family_size)
    floor = min_n_perm_for_alpha(corrected)
    headroom = min_n_perm_for_alpha(corrected / 2.0)
    return floor, headroom


def resolve_n_perm(family_size: int, alpha: float, requested: int) -> tuple[int, int]:
    floor, headroom = permutation_requirement(family_size, alpha)
    if int(requested) < floor:
        raise ValueError(
            f"n_perm={requested} cannot resolve alpha/family_size="
            f"{alpha / family_size:.3e} over {family_size} hypotheses; "
            f"need at least {floor}"
        )
    return floor, headroom


def _load_1080_min_n_perm():
    import importlib.util
    path = _THIS.with_name("regime_1080_unsupervised_vol_model.py")
    spec = importlib.util.spec_from_file_location("regime_1080_for_1940", path)
    mod = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(mod)
    return mod.min_n_perm_for_alpha


def num_token(value) -> str:
    if isinstance(value, bool) or not isinstance(value, (int, float)):
        raise ValueError(f"bad numeric token {value!r}")
    if isinstance(value, int):
        return str(value)
    return json.dumps(float(value))


def setting_id_adx(period: int, threshold) -> str:
    return f"adx|period={int(period)}|adx_threshold={num_token(threshold)}"


def setting_id_composite(period, return_eff, range_eff, efficiency, adx, cap) -> str:
    cap_token = "none" if cap is None else str(int(cap))
    return (
        f"composite|period={int(period)}|return_eff={num_token(return_eff)}"
        f"|range_eff={num_token(range_eff)}|efficiency={num_token(efficiency)}"
        f"|adx_threshold={num_token(adx)}|cap={cap_token}"
    )


def dataset_id(exchange: str, symbol: str, timeframe: str) -> str:
    return f"{exchange}|{symbol}|{timeframe}"


def cell_id(ds_id: str, split: str, horizon: int) -> str:
    return f"{ds_id}|{split}|h{int(horizon)}"


def parse_time_ms(text: str) -> int:
    ts = pd.Timestamp(text)
    if ts.tzinfo is None:
        ts = ts.tz_localize("UTC")
    else:
        ts = ts.tz_convert("UTC")
    return int(ts.value // 1_000_000)


def timeframe_ms(timeframe: str) -> int:
    if timeframe == "1h":
        return 3_600_000
    if timeframe == "4h":
        return 14_400_000
    raise ValueError(f"unsupported timeframe {timeframe!r}")


def _bound_text(name: str, which: str, oos_end: str) -> str:
    start, end = WINDOWS[name]
    if which == "start":
        raw = start
    elif name == "oos":
        raw = oos_end
    elif name == "2025H1":
        raw = WINDOWS["is"][0]
    else:
        raw = end
    text = str(raw)
    if "T" not in text:
        text = text + "T00:00:00Z"
    return text


def expand_protocol(protocol: dict) -> dict:
    settings = []
    for period in protocol["periods"]:
        for threshold in protocol["adx_thresholds"]:
            settings.append({
                "setting_id": setting_id_adx(period, threshold),
                "classifier": "adx",
                "period": int(period),
                "adx_threshold": float(threshold),
            })
    for period in protocol["periods"]:
        for ret in protocol["return_eff"]:
            for rng in protocol["range_eff"]:
                for eff in protocol["efficiency"]:
                    for adx in protocol["composite_adx_thresholds"]:
                        for cap in protocol["caps"]:
                            settings.append({
                                "setting_id": setting_id_composite(period, ret, rng, eff, adx, cap),
                                "classifier": "composite",
                                "period": int(period),
                                "return_eff": float(ret),
                                "range_eff": float(rng),
                                "efficiency": float(eff),
                                "adx_threshold": float(adx),
                                "cap": None if cap is None else int(cap),
                            })
    setting_ids = [row["setting_id"] for row in settings]
    if len(setting_ids) != len(set(setting_ids)):
        raise ValueError("duplicate setting identifier")
    datasets = []
    for row in protocol["datasets"]:
        datasets.append({
            "dataset_id": dataset_id(row["exchange"], row["symbol"], row["timeframe"]),
            "exchange": row["exchange"],
            "symbol": row["symbol"],
            "timeframe": row["timeframe"],
        })
    cells = []
    for ds in datasets:
        for split in protocol["splits"]:
            for horizon in protocol["horizons"]:
                cells.append({
                    "cell_id": cell_id(ds["dataset_id"], split, horizon),
                    "dataset_id": ds["dataset_id"],
                    "split": split,
                    "horizon": int(horizon),
                })
    inc = protocol["incumbents"]
    incumbent_ids = {
        "adx": setting_id_adx(inc["adx"]["period"], inc["adx"]["adx_threshold"]),
        "composite": setting_id_composite(
            inc["composite"]["period"], inc["composite"]["return_eff"],
            inc["composite"]["range_eff"], inc["composite"]["efficiency"],
            inc["composite"]["adx_threshold"], inc["composite"]["cap"]),
    }
    family_size = len(settings) * len(cells)
    return {
        "settings": settings,
        "setting_ids": setting_ids,
        "datasets": datasets,
        "cells": cells,
        "cell_ids": [row["cell_id"] for row in cells],
        "incumbent_ids": incumbent_ids,
        "family_size": family_size,
    }


def validate_protocol(protocol: dict) -> dict:
    if protocol.get("issue") != 1940 or protocol.get("no_fitted_model") is not True:
        raise ValueError("protocol is not the #1940 suggest-only study")
    if protocol.get("no_direction_target") is not True or protocol.get("target") != "forward_realized_vol":
        raise ValueError("protocol target must be forward realized volatility")
    if protocol.get("seed") != 0 or float(protocol.get("alpha")) != 0.05:
        raise ValueError("protocol seed or alpha does not match the freeze")
    if int(protocol.get("min_usable_bars")) != 200 or int(protocol.get("primary_horizon")) != 4:
        raise ValueError("protocol sample floor or primary horizon does not match the freeze")
    if list(protocol.get("horizons") or []) != [4, 12, 24]:
        raise ValueError("horizons must be h4, h12, h24")
    if int(protocol.get("block_dwell_multiplier")) != 3:
        raise ValueError("block dwell multiplier must be 3")
    if list(protocol.get("periods") or []) != [10, 14, 21, 28, 48, 60]:
        raise ValueError("period grid does not match the freeze")
    if list(protocol.get("adx_thresholds") or []) != [15, 20, 25, 30]:
        raise ValueError("ADX threshold grid does not match the freeze")
    if [float(x) for x in protocol.get("return_eff") or []] != [0.025, 0.05, 0.1]:
        raise ValueError("return-efficiency grid does not match the freeze")
    if [float(x) for x in protocol.get("range_eff") or []] != [0.015, 0.03, 0.06]:
        raise ValueError("range-efficiency grid does not match the freeze")
    if [float(x) for x in protocol.get("efficiency") or []] != [0.25, 0.5, 0.75]:
        raise ValueError("efficiency grid does not match the freeze")
    if list(protocol.get("composite_adx_thresholds") or []) != [15, 20, 25, 30]:
        raise ValueError("composite ADX threshold grid does not match the freeze")
    if list(protocol.get("caps") or []) != [14, None]:
        raise ValueError("caps must be 14 and uncapped")
    expected_ds = [(sym, tf, PLATFORM) for sym, tf in DATASETS]
    expected_ds += [("HYPE/USDC:USDC", "1h", "hyperliquid"), ("HYPE/USDC:USDC", "4h", "hyperliquid")]
    got_ds = [(row["symbol"], row["timeframe"], row["exchange"]) for row in protocol["datasets"]]
    if got_ds != expected_ds:
        raise ValueError("dataset list does not match eval_windows.DATASETS plus HYPE")
    oos_end = protocol["oos_end"]
    for name in WINDOWS:
        for which, index in (("start", 0), ("end", 1)):
            got = parse_time_ms(protocol["splits"][name][index])
            exp = parse_time_ms(_bound_text(name, which, oos_end))
            if got != exp:
                raise ValueError(f"split {name} {which} does not match the frozen windows")
    if protocol["splits"]["2025H1"][1] != protocol["splits"]["is"][0]:
        raise ValueError("2025H1 must end where is starts")
    if list(protocol.get("held_out_splits") or []) != ["oos", "2023", "2024", "2025H1"]:
        raise ValueError("held-out splits do not match the freeze")
    if protocol.get("selection_split") != "is":
        raise ValueError("selection split must be is")
    if list(protocol.get("selection_rank") or []) != ["kruskal_h_desc", "transition_rate_asc", "setting_id_asc"]:
        raise ValueError("selection rank does not match the freeze")
    if protocol.get("majority") != "strictly_more_than_half_of_available_held_out_cells":
        raise ValueError("majority rule does not match the freeze")
    if int(protocol.get("screen_n")) != 4999 or float(protocol.get("screen_continue_if_p_at_most")) != 0.0002:
        raise ValueError("screen rule does not match the freeze")
    if list(protocol.get("power_lam_grid") or []) != [0.0, 0.5, 1.0, 1.5, 2.0]:
        raise ValueError("power effect grid does not match #1211")
    if int(protocol.get("power_n_sim")) != 150 or float(protocol.get("power_target")) != 0.8:
        raise ValueError("power simulation total or target does not match #1211")
    if list(protocol.get("availability_order") or []) != ["missing", "invalid_price", "duplicate", "gapped", "truncated"]:
        raise ValueError("availability order does not match the freeze")
    expanded = expand_protocol(protocol)
    if expanded["family_size"] != int(protocol["family_size"]):
        raise ValueError(
            f"family_size {protocol['family_size']} != expanded {expanded['family_size']}"
        )
    for key, ident in expanded["incumbent_ids"].items():
        if ident not in expanded["setting_ids"]:
            raise ValueError(f"incumbent {key} is not a grid member: {ident}")
    floor, headroom = resolve_n_perm(expanded["family_size"], float(protocol["alpha"]), int(protocol["n_perm"]))
    if int(protocol["n_perm"]) != headroom:
        raise ValueError(f"n_perm must be the headroom total {headroom}, got {protocol['n_perm']}")
    if 1.0 / (int(protocol["n_perm"]) + 1) > float(protocol["alpha"]) / expanded["family_size"]:
        raise ValueError("n_perm does not resolve the corrected alpha")
    remote = _load_1080_min_n_perm()
    if remote(0.01) != min_n_perm_for_alpha(0.01) or remote(float(protocol["alpha"]) / expanded["family_size"]) != floor:
        raise ValueError("local permutation floor disagrees with the #1080 guard")
    if floor != min_n_perm_for_alpha(float(protocol["alpha"]) / expanded["family_size"]):
        raise ValueError("internal permutation floor mismatch")
    expanded["floor_n_perm"] = floor
    expanded["headroom_n_perm"] = headroom
    return expanded


def begin_study(protocol_path: str | Path) -> dict:
    path = Path(protocol_path)
    raw = path.read_bytes()
    protocol = json.loads(raw)
    expanded = validate_protocol(protocol)
    return {
        "sha256": hashlib.sha256(raw).hexdigest(),
        "protocol": protocol,
        "expanded": expanded,
        "path": str(path),
    }


def candle_hash(rows: list[tuple]) -> str:
    lines = [f"{int(ts)}|{o!r}|{h!r}|{l!r}|{c!r}|{v!r}" for ts, o, h, l, c, v in rows]
    return hashlib.sha256("\n".join(lines).encode()).hexdigest()


def _invalid_price(o, h, l, c, v) -> bool:
    vals = (o, h, l, c, v)
    if any(not math.isfinite(float(x)) for x in vals):
        return True
    if min(float(o), float(h), float(l), float(c)) <= 0 or float(v) < 0:
        return True
    if float(h) < float(l) or float(h) < max(float(o), float(c)) or float(l) > min(float(o), float(c)):
        return True
    return False


def last_completed_open(start_ms: int, end_ms: int, tf_ms: int) -> int | None:
    """Last candle open on the start grid whose bar is fully inside [start, end)."""
    if start_ms % tf_ms != 0 or end_ms - start_ms < tf_ms:
        return None
    candles = (end_ms - start_ms) // tf_ms
    return start_ms + (candles - 1) * tf_ms


def candle_defect(rows: list[tuple], start_ms: int, end_ms: int, tf_ms: int) -> str | None:
    if not rows:
        return "missing"
    if any(_invalid_price(*row[1:]) for row in rows):
        return "invalid_price"
    stamps = [int(row[0]) for row in rows]
    if len(stamps) != len(set(stamps)):
        return "duplicate"
    if any(stamps[i] >= stamps[i + 1] or stamps[i + 1] - stamps[i] != tf_ms or stamps[i] % tf_ms != 0 for i in range(len(stamps) - 1)):
        return "gapped"
    if stamps[0] % tf_ms != 0:
        return "gapped"
    last = last_completed_open(start_ms, end_ms, tf_ms)
    if last is None:
        return "truncated"
    expected_n = (last - start_ms) // tf_ms + 1
    if stamps[0] != start_ms or stamps[-1] != last or len(stamps) != expected_n:
        return "truncated"
    return None


def refuse_wal(path: Path) -> None:
    for suffix in ("-wal", "-shm"):
        if Path(str(path) + suffix).exists():
            raise RuntimeError(f"snapshot {path} has an unresolved write-ahead-log sibling {suffix}")


def open_snapshot(path: str | Path) -> sqlite3.Connection:
    src = Path(path).resolve()
    if not src.is_file():
        raise RuntimeError(f"snapshot does not exist: {src}")
    refuse_wal(src)
    uri = src.as_uri() + "?mode=ro&immutable=1"
    return sqlite3.connect(uri, uri=True)


def read_candles(conn, exchange: str, symbol: str, timeframe: str, start_ms: int, end_ms: int) -> list[tuple]:
    tf_ms = timeframe_ms(timeframe)
    query = (
        "SELECT timestamp, open, high, low, close, volume FROM ohlcv "
        "WHERE exchange=? AND symbol=? AND timeframe=? AND timestamp>=? AND timestamp<=? "
        "ORDER BY timestamp ASC"
    )
    last_open = last_completed_open(int(start_ms), int(end_ms), tf_ms)
    if last_open is None:
        return []
    rows = conn.execute(query, (exchange, symbol, timeframe, int(start_ms), int(last_open))).fetchall()
    return [(int(ts), float(o), float(h), float(l), float(c), float(v)) for ts, o, h, l, c, v in rows]


def describe_input(rows, exchange, symbol, timeframe, start_ms, end_ms) -> dict:
    tf_ms = timeframe_ms(timeframe)
    reason = candle_defect(rows, start_ms, end_ms, tf_ms)
    observed = [int(row[0]) for row in rows]
    return {
        "exchange": exchange,
        "symbol": symbol,
        "timeframe": timeframe,
        "requested_start_ms": int(start_ms),
        "requested_end_ms": int(end_ms),
        "observed_start_ms": observed[0] if observed else None,
        "observed_end_ms": observed[-1] if observed else None,
        "raw_rows": len(rows),
        "accepted_rows": 0 if reason else len(rows),
        "content_sha256": candle_hash(rows),
        "status": "unavailable" if reason else "available",
        "reason": reason,
    }


def file_sha256(path: Path) -> str:
    digest = hashlib.sha256()
    with path.open("rb") as fh:
        for chunk in iter(lambda: fh.read(1 << 20), b""):
            digest.update(chunk)
    return digest.hexdigest()


def support_runs(mask: np.ndarray) -> list[tuple[int, int]]:
    idx = np.flatnonzero(mask)
    if len(idx) == 0:
        return []
    cuts = np.where(np.diff(idx) != 1)[0] + 1
    runs = []
    cursor = 0
    for group in np.split(idx, cuts):
        runs.append((cursor, cursor + len(group)))
        cursor += len(group)
    return runs


def forward_window_inside(mask: np.ndarray, horizon: int) -> bool:
    idx = np.flatnonzero(mask)
    return bool(len(idx) == 0 or np.all(idx + int(horizon) < len(mask)))


def gapped_stability(labels: np.ndarray, mask: np.ndarray) -> dict:
    idx = np.flatnonzero(mask)
    if len(idx) == 0:
        return stability(np.array([""], dtype=object))
    cuts = np.where(np.diff(idx) != 1)[0] + 1
    parts = []
    for group in np.split(idx, cuts):
        parts.append(np.asarray(labels[group], dtype=object))
        parts.append(np.array([""], dtype=object))
    seq = np.concatenate(parts[:-1])
    return stability(seq)


def adx_label_array(adx, plus, minus, ready, threshold: float) -> np.ndarray:
    finite = np.asarray(ready, dtype=bool) & np.isfinite(adx) & np.isfinite(plus) & np.isfinite(minus)
    labels = np.full(len(adx), "", dtype=object)
    high = adx >= float(threshold)
    up = plus > minus
    down = minus > plus
    labels[finite & ~high] = "ranging"
    labels[finite & high & up] = "trending_up"
    labels[finite & high & down] = "trending_down"
    labels[finite & high & ~up & ~down] = "ranging"
    return labels


def composite_label_array(features: np.ndarray, thresholds: dict) -> np.ndarray:
    ret = features[:, 0]
    rng = features[:, 1]
    eff = features[:, 2]
    adx = features[:, 3]
    finite = np.isfinite(features).all(axis=1)
    labels = np.full(len(features), "", dtype=object)
    if not finite.any():
        return labels
    ret_th = float(thresholds["return_eff"])
    range_th = float(thresholds["range_eff"])
    adx_th = float(thresholds["adx"])
    eff_th = float(thresholds["efficiency"])
    big = np.abs(ret) >= ret_th
    up = ret > 0
    high = adx >= adx_th
    wide = rng >= range_th
    clean = (eff >= eff_th) & high
    idx = np.flatnonzero(finite)
    chosen = np.empty(len(idx), dtype=object)
    sub = big[idx]
    sub_up = up[idx]
    sub_clean = clean[idx]
    sub_high = high[idx]
    sub_wide = wide[idx]
    sub_ret = ret[idx]
    chosen[:] = "ranging_quiet"
    volatile = ~sub & ~sub_high & sub_wide
    chosen[volatile] = "ranging_volatile"
    directional = ~sub & sub_high
    chosen[directional] = "ranging_directional"
    chosen[directional & (sub_ret > 0)] = "ranging_directional_up"
    chosen[directional & (sub_ret < 0)] = "ranging_directional_down"
    down = sub & ~sub_up
    chosen[down & sub_clean] = "trending_down_clean"
    chosen[down & ~sub_clean] = "trending_down_choppy"
    up_big = sub & sub_up
    chosen[up_big & sub_clean] = "trending_up_clean"
    chosen[up_big & ~sub_clean] = "trending_up_choppy"
    labels[idx] = chosen
    return labels


def assert_composite_matches_mapper(features: np.ndarray, thresholds: dict, labels: np.ndarray, stride: int = 17) -> None:
    finite = np.flatnonzero(np.isfinite(features).all(axis=1))
    if len(finite) == 0:
        return
    for pos in finite[::stride]:
        got = map_composite_label(float(features[pos, 0]), float(features[pos, 3]),
                                  float(features[pos, 1]), float(features[pos, 2]), thresholds)
        if got != labels[pos]:
            raise RuntimeError(f"composite label mismatch at {pos}: mapper {got} array {labels[pos]}")


def tie_inv(values: np.ndarray) -> float:
    n = len(values)
    if n <= 1:
        return 1.0
    _, counts = np.unique(np.asarray(values, dtype=float), return_counts=True)
    ties = float((counts ** 3 - counts).sum())
    scale = 1.0 - ties / (n ** 3 - n)
    return 1.0 / scale if scale > 0 else 1.0


def coverage_table(labels: np.ndarray, known: tuple[str, ...]) -> dict:
    total = len(labels)
    counts = {name: 0 for name in known}
    if total:
        values, freq = np.unique(labels, return_counts=True)
        for value, freq_i in zip(values, freq):
            key = str(value)
            if key not in counts:
                raise RuntimeError(f"unexpected label {key}")
            counts[key] = int(freq_i)
    return {name: {"count": counts[name], "pct": (counts[name] / total if total else 0.0)} for name in known}


def vol_summaries(labels: np.ndarray, fwd: np.ndarray, known: tuple[str, ...]) -> tuple[dict, dict, list, list]:
    means, medians = {}, {}
    for name in known:
        vals = fwd[labels == name]
        means[name] = None if len(vals) == 0 else float(np.mean(vals))
        medians[name] = None if len(vals) == 0 else float(np.median(vals))
    present = [name for name in known if means[name] is not None]
    by_mean = sorted(present, key=lambda name: (means[name], name))
    by_median = sorted(present, key=lambda name: (medians[name], name))
    return means, medians, by_mean, by_median


def block_length(stab: dict, horizon: int, multiplier: int) -> int:
    dwells = list((stab.get("mean_dwell") or {}).values())
    mean_dwell = float(np.mean(dwells)) if dwells else 1.0
    return max(int(multiplier * mean_dwell), int(horizon), 1)


def as_gate_report(row: dict) -> dict:
    key = f"h{int(row['horizon'])}"
    return {
        "target": "volatility",
        "stability": {"transition_rate": float(row["transition_rate"])},
        key: {
            "separation": {"kruskal_h": float(row["kruskal_h"])},
            "significance": {"p_value": float(row["p_value"])},
        },
    }


def majority_passes(n_pass: int, n_available: int) -> bool:
    return n_available > 0 and (int(n_pass) * 2) > int(n_available)


def select_winners(rows: list[dict], incumbent_ids: dict, dataset_ids: list[str] | None = None) -> list[dict]:
    training = [row for row in rows if row.get("split") == "is" and int(row.get("horizon")) == 4 and row.get("status", "available") == "available"]
    winners = []
    datasets = list(dataset_ids) if dataset_ids is not None else sorted({row["dataset_id"] for row in training})
    for ds in datasets:
        for classifier, incumbent in incumbent_ids.items():
            group = [row for row in training if row["dataset_id"] == ds and row["classifier"] == classifier]
            inc = next((row for row in group if row["setting_id"] == incumbent), None)
            pool = []
            if inc is not None:
                for row in group:
                    if row["setting_id"] == incumbent:
                        continue
                    if row.get("support_sha256") != inc.get("support_sha256"):
                        continue
                    gate = gate_verdict(as_gate_report(inc), as_gate_report(row), primary="h4")
                    if gate["ship"]:
                        pool.append(row)
            pool.sort(key=lambda row: (-float(row["kruskal_h"]), float(row["transition_rate"]), row["setting_id"]))
            winners.append({
                "dataset_id": ds,
                "classifier": classifier,
                "incumbent_id": incumbent,
                "setting_id": None if not pool else pool[0]["setting_id"],
                "support_sha256": None if inc is None else inc.get("support_sha256"),
                "n_gate_pass": len(pool),
            })
    return winners


def adjusted_p_values(pvals: list[float], family_size: int) -> np.ndarray:
    p = np.asarray(pvals, dtype=float)
    m = len(p)
    adj = np.ones(m, dtype=float)
    if m == 0:
        return adj
    order = np.argsort(p, kind="mergesort")
    running = 1.0
    for rank in range(m, 0, -1):
        idx = int(order[rank - 1])
        running = min(running, float(p[idx]) * float(family_size) / float(rank))
        adj[idx] = min(1.0, running)
    return adj


def apply_bh(rows: list[dict], family_size: int, alpha: float) -> None:
    indexed = [row for row in rows if row.get("status", "available") == "available" and "p_value" in row]
    pvals = [float(row["p_value"]) for row in indexed]
    flags = benjamini_hochberg(pvals, alpha=alpha, family_size=family_size) if pvals else []
    adj = adjusted_p_values(pvals, family_size)
    for row, flag, p_adj in zip(indexed, flags, adj):
        row["bh_reject"] = bool(flag)
        row["p_adjusted"] = float(p_adj)
        row["corrected_pass"] = bool(flag) and float(p_adj) <= float(alpha)


def cell_decision(candidate: dict, incumbent: dict, alpha: float) -> dict:
    gate = gate_verdict(as_gate_report(incumbent), as_gate_report(candidate), primary=f"h{int(candidate['horizon'])}")
    corrected = bool(candidate.get("corrected_pass"))
    return {
        "gate_ship": bool(gate["ship"]),
        "corrected_pass": corrected,
        "passes": bool(gate["ship"]) and corrected,
        "raw_pass_corrected_fail": bool(gate["ship"]) and not corrected,
    }


def decide_suggestions(winners: list[dict], rows: list[dict], alpha: float, held_out_splits: list[str], horizons: list[int]) -> list[dict]:
    by_key = {(row["dataset_id"], row["split"], int(row["horizon"]), row["setting_id"]): row for row in rows if row.get("status", "available") == "available"}
    out = []
    for winner in winners:
        ds = winner["dataset_id"]
        setting = winner["setting_id"]
        incumbent = winner["incumbent_id"]
        available, missing, passing, raw_corrected = [], [], [], []
        for split in held_out_splits:
            for horizon in horizons:
                key = (ds, split, int(horizon), setting) if setting else None
                inc_key = (ds, split, int(horizon), incumbent)
                cand = by_key.get(key) if key else None
                inc = by_key.get(inc_key)
                slot = {"split": split, "horizon": int(horizon)}
                if setting is None or cand is None or inc is None or cand.get("support_sha256") != inc.get("support_sha256"):
                    missing.append({**slot, "reason": "unavailable"})
                    continue
                decision = cell_decision(cand, inc, alpha)
                slot.update(decision)
                slot["kruskal_h"] = cand["kruskal_h"]
                slot["p_value"] = cand["p_value"]
                slot["p_adjusted"] = cand.get("p_adjusted")
                available.append(slot)
                if decision["passes"]:
                    passing.append(slot)
                if decision["raw_pass_corrected_fail"]:
                    raw_corrected.append(slot)
        n_avail = len(available)
        suggested = setting is not None and majority_passes(len(passing), n_avail)
        reason = "selected"
        if setting is None:
            reason = "no training winner"
            suggested = False
        elif n_avail == 0:
            reason = "no available held-out cells"
            suggested = False
        elif raw_corrected and not passing:
            reason = "raw gate passed and Benjamini-Hochberg correction failed"
            suggested = False
        elif not majority_passes(len(passing), n_avail):
            reason = "held-out majority not met"
            suggested = False
        out.append({
            "dataset_id": ds,
            "classifier": winner["classifier"],
            "setting_id": setting,
            "incumbent_id": incumbent,
            "suggested": bool(suggested),
            "reason": reason,
            "held_out_available": n_avail,
            "held_out_passing": len(passing),
            "held_out_missing": missing,
            "passing_cells": [{"split": item["split"], "horizon": item["horizon"]} for item in passing],
            "raw_pass_corrected_fail": len(raw_corrected),
        })
    return out


def detection_limit(lam_grid, powers: dict, labels, fwd, profile, target: float) -> dict:
    from_1211 = _load_1211()
    limit = from_1211.min_detectable_effect(lam_grid, powers, target=target)
    curve = {num_token(lam): float(powers[float(lam)]) for lam in lam_grid}
    if limit is None:
        top = float(max(lam_grid))
        planted = from_1211.inject_effect(labels, fwd, profile, top)
        bound_h = float(separation(np.asarray(labels, dtype=object), planted)["kruskal_h"])
        return {
            "status": "unresolved",
            "detection_limit_multiplier": None,
            "detection_limit_kruskal_h": None,
            "bound_multiplier": top,
            "bound_power": float(powers[top]),
            "bound_kruskal_h": bound_h,
            "power_curve": curve,
        }
    planted = from_1211.inject_effect(labels, fwd, profile, float(limit))
    measured = float(separation(np.asarray(labels, dtype=object), planted)["kruskal_h"])
    return {
        "status": "resolved",
        "detection_limit_multiplier": float(limit),
        "detection_limit_kruskal_h": measured,
        "bound_multiplier": None,
        "bound_power": None,
        "bound_kruskal_h": None,
        "power_curve": curve,
    }


def _load_1211():
    import importlib.util
    path = _THIS.with_name("regime_1211_incumbent_baseline.py")
    spec = importlib.util.spec_from_file_location("regime_1211_for_1940", path)
    mod = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(mod)
    return mod


class Kernel:
    def __init__(self, path: str):
        lib = ctypes.CDLL(path)
        self._runs = lib.count_block_exceedances_runs
        self._runs.argtypes = [
            ctypes.POINTER(ctypes.c_int32), ctypes.POINTER(ctypes.c_double),
            ctypes.c_int, ctypes.c_int, ctypes.POINTER(ctypes.c_int32), ctypes.POINTER(ctypes.c_int32),
            ctypes.c_int, ctypes.c_int, ctypes.c_double, ctypes.c_double, ctypes.c_uint64,
            ctypes.c_int64, ctypes.c_int64, ctypes.POINTER(ctypes.c_int64),
            ctypes.POINTER(ctypes.c_int64), ctypes.POINTER(ctypes.c_double),
        ]
        self._order = lib.h_for_block_order
        self._order.restype = ctypes.c_double
        self._order.argtypes = [
            ctypes.POINTER(ctypes.c_int32), ctypes.POINTER(ctypes.c_double),
            ctypes.c_int, ctypes.c_int, ctypes.c_int, ctypes.POINTER(ctypes.c_int32),
            ctypes.c_int, ctypes.c_double,
        ]

    def count(self, labels, ranks, run_lo, run_hi, k, block_len, h_obs, inv_tie, seed, n_perm, stop_ge):
        labels = np.ascontiguousarray(labels, dtype=np.int32)
        ranks = np.ascontiguousarray(ranks, dtype=np.float64)
        run_lo = np.ascontiguousarray(run_lo, dtype=np.int32)
        run_hi = np.ascontiguousarray(run_hi, dtype=np.int32)
        ge = ctypes.c_int64()
        executed = ctypes.c_int64()
        observed = ctypes.c_double()
        self._runs(
            labels.ctypes.data_as(ctypes.POINTER(ctypes.c_int32)),
            ranks.ctypes.data_as(ctypes.POINTER(ctypes.c_double)),
            int(len(labels)), int(k),
            run_lo.ctypes.data_as(ctypes.POINTER(ctypes.c_int32)),
            run_hi.ctypes.data_as(ctypes.POINTER(ctypes.c_int32)),
            int(len(run_lo)), int(block_len), float(h_obs), float(inv_tie), ctypes.c_uint64(int(seed) % 2**64),
            int(n_perm), int(stop_ge), ctypes.byref(ge), ctypes.byref(executed), ctypes.byref(observed),
        )
        return int(ge.value), int(executed.value), float(observed.value)

    def identity_h(self, labels, ranks, k, block_len, inv_tie) -> float:
        n = len(labels)
        nb = (n + block_len - 1) // block_len
        order = np.arange(nb, dtype=np.int32)
        return float(self._order(
            labels.ctypes.data_as(ctypes.POINTER(ctypes.c_int32)),
            ranks.ctypes.data_as(ctypes.POINTER(ctypes.c_double)),
            n, int(k), int(block_len), order.ctypes.data_as(ctypes.POINTER(ctypes.c_int32)), nb, float(inv_tie),
        ))


def compile_kernel(build_dir: Path) -> Path:
    src = _THIS.with_name("regime_1940_block_perm.c")
    digest = hashlib.sha256(src.read_bytes()).hexdigest()[:16]
    out = build_dir / f"regime_1940_block_perm_{digest}.dylib"
    flag = ["-dynamiclib"] if sys.platform == "darwin" else ["-shared", "-fPIC"]
    subprocess.check_call(["cc", "-O3", *flag, "-o", str(out), str(src)], stdout=subprocess.DEVNULL)
    return out


def self_check_kernel(kernel: Kernel) -> None:
    rng = np.random.default_rng(0)
    for n, block, k, ties in ((40, 4, 9, False), (41, 6, 3, True), (80, 5, 9, False)):
        labels = rng.integers(0, k, n).astype(np.int32)
        fwd = rng.integers(0, 15, n).astype(float) if ties else rng.random(n)
        ranks = rank_data(fwd)
        inv = tie_inv(fwd)
        groups = [fwd[labels == code] for code in range(k) if np.any(labels == code)]
        py_h = float(__import__("regime_stats").kruskal_h(groups))
        c_h = kernel.identity_h(labels, ranks, k, block, inv)
        if abs(py_h - c_h) > 1e-6:
            raise RuntimeError(f"kernel H {c_h} != Kruskal {py_h}")
        lo = np.array([0], dtype=np.int32)
        hi = np.array([n], dtype=np.int32)
        ge, executed, observed = kernel.count(labels, ranks, lo, hi, k, block, -1.0, inv, 3, 25, 0)
        if executed != 25 or abs(observed - py_h) > 1e-6:
            raise RuntimeError("kernel self-check did not finish the reference stream")
        if ge < 0:
            raise RuntimeError("negative exceedance count")


def _init_worker(path: str) -> None:
    global _KERNEL
    _KERNEL = Kernel(path)


_POWER = None


def _init_power_worker(path: str) -> None:
    global _POWER
    _init_worker(path)
    _POWER = _load_1211()


def _checkpoint_path(root, cell_id: str) -> Path:
    return Path(root) / (cell_id.replace("|", "__") + ".json")


def _load_checkpoint(args, cell_id: str):
    if not getattr(args, "checkpoint_dir", None):
        return None
    path = _checkpoint_path(args.checkpoint_dir, cell_id)
    if not path.exists():
        return None
    doc = json.loads(path.read_text())
    expected = {
        "protocol_sha256": getattr(args, "protocol_sha", None),
        "snapshot_sha256": getattr(args, "snapshot_sha", None),
        "kernel_sha256": getattr(args, "kernel_sha", None),
    }
    if any(doc.get(key) != value for key, value in expected.items()):
        return None
    return doc


def _save_checkpoint(args, cell_id: str, cell_scores, population) -> None:
    if not getattr(args, "checkpoint_dir", None):
        return
    path = _checkpoint_path(args.checkpoint_dir, cell_id)
    path.parent.mkdir(parents=True, exist_ok=True)
    path.write_text(json.dumps(_plain({
        "protocol_sha256": args.protocol_sha,
        "snapshot_sha256": args.snapshot_sha,
        "kernel_sha256": args.kernel_sha,
        "scores": cell_scores,
        "population": population,
    }), separators=(",", ":"), allow_nan=False))


def _power_sim(task: dict) -> int:
    seed = int(task["seed"])
    rng = np.random.default_rng(seed)
    null_fwd = _POWER.block_bootstrap(task["fwd"], task["block_len"], len(task["fwd"]), rng)
    planted = _POWER.inject_effect(task["labels"], null_fwd, task["profile"], float(task["lam"]))
    labels = np.asarray(task["labels"], dtype=object)
    planted = np.asarray(planted, dtype=float)
    sep_h = float(separation(labels, planted)["kruskal_h"])
    if sep_h == 0.0:
        return 0
    codes = codes_for(labels, tuple(task["known"]))
    ranks = rank_data(planted)
    lo = np.array([0], dtype=np.int32)
    hi = np.array([len(codes)], dtype=np.int32)
    ge, executed, _obs = _KERNEL.count(
        codes, ranks, lo, hi, len(task["known"]), task["block_len"], sep_h, tie_inv(planted),
        seed, task["n_perm"], 2,
    )
    if executed == task["n_perm"] and ge <= 1 and (ge + 1) / (task["n_perm"] + 1) <= task["alpha"] / task["family"]:
        return 1
    return 0


def _perm_task(task: dict) -> list[dict]:
    kernel = _KERNEL
    ranks = task["ranks"]
    run_lo = task["run_lo"]
    run_hi = task["run_hi"]
    inv = task["inv_tie"]
    out = []
    for item in task["items"]:
        labels = item["labels"]
        if item["degenerate"]:
            out.append({"ge": item["n_perm"], "executed": 0, "h_obs": 0.0, "kind": "degenerate", "p_value": 1.0})
            continue
        ge_s, ex_s, obs_s = kernel.count(
            labels, ranks, run_lo, run_hi, item["k"], item["block_len"], -1.0, inv,
            item["seed"], task["screen_n"], task["screen_stop"],
        )
        screen_p = (ge_s + 1) / (ex_s + 1)
        if not task.get("allow_full", True) or screen_p > task["screen_max_p"]:
            out.append({"ge": ge_s, "executed": ex_s, "h_obs": float(obs_s), "kind": "screen", "p_value": float(screen_p)})
            continue
        ge, ex, obs = kernel.count(
            labels, ranks, run_lo, run_hi, item["k"], item["block_len"], float(obs_s), inv,
            item["seed"], task["n_perm"], task["full_stop"],
        )
        if ex < task["n_perm"]:
            p_value = (ge + 1) / (ex + 1)
            kind = "exceeds_alpha"
        else:
            p_value = (ge + 1) / (task["n_perm"] + 1)
            kind = "full"
        out.append({"ge": ge, "executed": ex, "h_obs": float(obs), "kind": kind, "p_value": float(p_value)})
    return out


def _validate_kernel_h(perm, stats, setting_id: str, cell_id: str) -> None:
    if abs(float(perm["h_obs"]) - float(stats["kruskal_h"])) > 1e-5 and perm["kind"] != "degenerate":
        raise RuntimeError(f"kernel H drifted for {setting_id} {cell_id}")


def series_seed(master: int, key: str) -> int:
    digest = hashlib.sha256(f"{int(master)}|{key}".encode()).digest()
    value = int.from_bytes(digest[:8], "little")
    return value or 1


def codes_for(labels: np.ndarray, known: tuple[str, ...]) -> np.ndarray:
    table = {name: i for i, name in enumerate(known)}
    out = np.empty(len(labels), dtype=np.int32)
    for i, label in enumerate(labels):
        out[i] = table[str(label)]
    return out


def frame_from_rows(rows: list[tuple]) -> pd.DataFrame:
    return pd.DataFrame({
        "open": [row[1] for row in rows],
        "high": [row[2] for row in rows],
        "low": [row[3] for row in rows],
        "close": [row[4] for row in rows],
        "volume": [row[5] for row in rows],
    })


def build_label_bank(frame: pd.DataFrame, settings: list[dict]) -> dict:
    periods = sorted({int(row["period"]) for row in settings})
    adx_parts = {}
    for period in periods:
        reg = compute_regime(frame, period=period, adx_threshold=20.0)
        adx_parts[period] = {
            "adx": reg["adx"].to_numpy(dtype=float),
            "plus": reg["plus_di"].to_numpy(dtype=float),
            "minus": reg["minus_di"].to_numpy(dtype=float),
            "ready": reg["regime_ready"].to_numpy(dtype=bool),
        }
        reference = reg["regime"].to_numpy()
        built = adx_label_array(adx_parts[period]["adx"], adx_parts[period]["plus"], adx_parts[period]["minus"], adx_parts[period]["ready"], 20.0)
        if not np.array_equal(reference, built):
            raise RuntimeError(f"ADX label rebuild mismatch at period {period}")
    features = {}
    caps = sorted({("none" if row.get("cap") is None else int(row["cap"])) for row in settings if row["classifier"] == "composite"}, key=lambda item: (item == "none", item))
    for period in periods:
        for cap_token in caps:
            cap = None if cap_token == "none" else int(cap_token)
            matrix = composite_feature_matrix(frame, period, adx_period_cap=COMPOSITE_ADX_PERIOD_CAP if cap == 14 else cap)
            features[(period, cap_token)] = matrix.to_numpy(dtype=float)
    if (48, 14) in features:
        incumbent_th = {"return_eff": 0.05, "range_eff": 0.03, "efficiency": 0.5, "adx": 25.0}
        direct = compute_regime_composite(frame, period=48, thresholds=incumbent_th, adx_period_cap=14)["regime"].to_numpy()
        rebuilt = composite_label_array(features[(48, 14)], incumbent_th)
        if not np.array_equal(direct, rebuilt):
            raise RuntimeError("composite incumbent rebuild mismatch")
        assert_composite_matches_mapper(features[(48, 14)], incumbent_th, rebuilt)
    bank = {}
    base = np.ones(len(frame), dtype=bool)
    for period, part in adx_parts.items():
        finite = part["ready"] & np.isfinite(part["adx"]) & np.isfinite(part["plus"]) & np.isfinite(part["minus"])
        base &= finite
    for feat in features.values():
        base &= np.isfinite(feat).all(axis=1)
    for row in settings:
        if row["classifier"] == "adx":
            part = adx_parts[row["period"]]
            labels = adx_label_array(part["adx"], part["plus"], part["minus"], part["ready"], row["adx_threshold"])
            known = ADX_LABELS
        else:
            cap_token = "none" if row["cap"] is None else int(row["cap"])
            th = {"return_eff": row["return_eff"], "range_eff": row["range_eff"], "efficiency": row["efficiency"], "adx": row["adx_threshold"]}
            labels = composite_label_array(features[(row["period"], cap_token)], th)
            known = COMPOSITE_LABELS
        bank[row["setting_id"]] = {"labels": labels, "known": known, "classifier": row["classifier"]}
    return {"bank": bank, "base": base, "close": frame["close"].to_numpy(dtype=float)}


def score_cell_stats(cell, bank_row, mask, close, multiplier: int) -> dict:
    labels = bank_row["labels"]
    kept = labels[mask]
    fwd_full = forward_realized_vol(close, int(cell["horizon"]))
    fwd = fwd_full[mask]
    sep = separation(kept, fwd)
    stab = gapped_stability(labels, mask)
    known = bank_row["known"]
    cov = coverage_table(kept, known)
    means, medians, by_mean, by_median = vol_summaries(kept, fwd, known)
    present = [name for name, info in cov.items() if info["count"] > 0]
    return {
        "kruskal_h": float(sep["kruskal_h"]),
        "transition_rate": float(stab["transition_rate"]),
        "flips": int(stab["flips"]),
        "mean_dwell": {key: float(value) for key, value in sorted((stab.get("mean_dwell") or {}).items())},
        "coverage": cov,
        "mean_vol": means,
        "median_vol": medians,
        "order_by_mean": by_mean,
        "order_by_median": by_median,
        "block_len": block_length(stab, cell["horizon"], multiplier),
        "degenerate": len(present) < 2 or float(sep["kruskal_h"]) == 0.0,
        "k": len(known),
        "known": known,
    }


def render_report(doc: dict) -> str:
    lines = ["# Regime classifier calibration (#1940)", ""]
    lines.append("## Verdict")
    lines.append("")
    lines.append("Verdict: suggest-only. This study does not change live defaults, config, or certification.")
    lines.append("")
    lines.append(
        f"Protocol sha256 `{doc.get('protocol_sha256')}`. Family size {doc.get('family_size')}. "
        f"Permutation total {doc.get('n_perm')} with screen {doc.get('screen_n')} at continue-at-most "
        f"{doc.get('screen_continue_if_p_at_most')}. Alpha {doc.get('alpha')}."
    )
    lines.append("")
    suggested = [row for row in doc.get("suggestions") or [] if row.get("suggested")]
    if not suggested:
        lines.append("No setting is suggested.")
    for row in doc.get("suggestions") or []:
        lines.append(
            f"- {row['dataset_id']} {row['classifier']}: suggested={row['suggested']} "
            f"setting={row.get('setting_id')} reason={row['reason']} "
            f"held-out {row['held_out_passing']}/{row['held_out_available']} "
            f"missing {len(row.get('held_out_missing') or [])} "
            f"raw-pass-corrected-fail {row.get('raw_pass_corrected_fail')}"
        )
    lines.append("")
    lines.append("## Inputs")
    lines.append("")
    for row in doc.get("inputs") or []:
        lines.append(
            f"- {row['cell_id']}: {row['status']} {row.get('reason') or ''} "
            f"raw={row['raw_rows']} accepted={row['accepted_rows']} usable={row.get('usable_rows')} "
            f"hash={row['content_sha256']}"
        )
    lines.append("")
    lines.append("## ADX forward-volatility separation and stability")
    lines.append("")
    lines.append("ADX labels are scored on the common timestamp support. Kruskal-Wallis H, transition rate, flips, and mean dwell are in the results.")
    for row in doc.get("adx_highlights") or []:
        lines.append(
            f"- {row['cell_id']} {row['setting_id']}: H={row['kruskal_h']:.6g} p={row['p_value']:.6g} "
            f"transition={row['transition_rate']:.6g} flips={row['flips']} "
            f"detection={_limit_text(row.get('detection'))}"
        )
    lines.append("")
    lines.append("## ADX versus composite")
    lines.append("")
    for row in doc.get("comparisons") or []:
        lines.append(f"- {row['dataset_id']} {row['split']}: {row['text']}")
    lines.append("")
    lines.append("## Confusion")
    lines.append("")
    for row in doc.get("confusion") or []:
        lines.append(f"- {row['name']}: `{json.dumps(row['counts'], sort_keys=True)}`")
    lines.append("")
    lines.append("## Threshold and period sensitivity")
    lines.append("")
    for row in doc.get("sensitivity") or []:
        lines.append(f"- {row}")
    lines.append("")
    lines.append("## ADX cap")
    lines.append("")
    lines.append("Issue 1939 `required_regime_bars` is reused when the cap is 14. Uncapped readiness uses max(period, 2*period+1) because that helper's cap is fixed.")
    for row in doc.get("cap_agreement") or []:
        lines.append(f"- {row['dataset_id']} {row['split']} {row['pair']}: agreement={row['agreement']:.6g}")
    lines.append("")
    lines.append("## Asset and timeframe differences")
    lines.append("")
    for row in doc.get("asset_notes") or []:
        lines.append(f"- {row}")
    lines.append("")
    lines.append("## Flips and dwell")
    lines.append("")
    for row in doc.get("dwell_highlights") or []:
        lines.append(f"- {row['cell_id']} {row['setting_id']}: flips={row['flips']} dwell={json.dumps(row['mean_dwell'], sort_keys=True)}")
    lines.append("")
    lines.append("## Label volatility ordering")
    lines.append("")
    for row in doc.get("order_highlights") or []:
        lines.append(
            f"- {row['cell_id']} {row['setting_id']}: mean {row['order_by_mean']} median {row['order_by_median']} "
            f"quiet_below_volatile={row.get('quiet_below_volatile')}"
        )
    lines.append("")
    lines.append("## Detection limits")
    lines.append("")
    lines.append("The multiplier is the injected effect scale. Kruskal-Wallis H is measured after injection. The multiplier is not Kruskal-Wallis H.")
    for row in doc.get("power") or []:
        if row.get("status") == "unresolved":
            lines.append(
                f"- {row['dataset_id']} {row['classifier']} h{row['horizon']}: detection limit unresolved. "
                f"The largest tested multiplier is {row['bound_multiplier']}, where power is {row['bound_power']}. "
                f"The Kruskal-Wallis H at that multiplier is {row['bound_kruskal_h']}."
            )
        elif row.get("status") == "unavailable":
            lines.append(f"- {row['dataset_id']} {row['classifier']} h{row['horizon']}: detection limit unavailable ({row.get('reason')}).")
        else:
            lines.append(
                f"- {row['dataset_id']} {row['classifier']} h{row['horizon']}: "
                f"multiplier {row['detection_limit_multiplier']:.6g}; "
                f"Kruskal-Wallis H {row['detection_limit_kruskal_h']:.6g}."
            )
    lines.append("")
    lines.append("## Nulls")
    lines.append("")
    lines.append("Each null below is a scored cell that did not reject after correction. The detection limit for its training profile is beside it.")
    for row in doc.get("nulls") or []:
        lines.append(f"- {row['setting_id']} {row['cell_id']}: H={row['kruskal_h']:.6g} p={row['p_value']:.6g} {_limit_text(row.get('detection'))}")
    lines.append("")
    return "\n".join(lines)


def _limit_brief(limit) -> dict:
    if not limit:
        return {"status": "unavailable", "reason": "no training profile"}
    keys = ("status", "reason", "detection_limit_multiplier", "detection_limit_kruskal_h",
            "bound_multiplier", "bound_power", "bound_kruskal_h")
    return {key: limit.get(key) for key in keys}


def _limit_text(limit) -> str:
    if not limit:
        return "detection limit unavailable"
    if limit.get("status") == "unresolved":
        return (
            f"detection limit unresolved; largest multiplier {limit.get('bound_multiplier')}; "
            f"Kruskal-Wallis H at that multiplier {limit.get('bound_kruskal_h')}"
        )
    if limit.get("status") != "resolved":
        return f"detection limit unavailable ({limit.get('reason')})"
    return (
        f"detection multiplier {limit.get('detection_limit_multiplier')}; "
        f"Kruskal-Wallis H {limit.get('detection_limit_kruskal_h')}"
    )


def build_parser() -> argparse.ArgumentParser:
    parser = argparse.ArgumentParser(description="Suggest-only ADX and composite calibration (#1940)")
    parser.add_argument("--protocol", default=str(PROTOCOL_PATH))
    parser.add_argument("--snapshot", default=None)
    parser.add_argument("--results", default=str(RESULTS_PATH))
    parser.add_argument("--report", default=str(REPORT_PATH))
    parser.add_argument("--checkpoint-dir", default=None)
    parser.add_argument("--workers", type=int, default=max(1, os.cpu_count() or 1))
    parser.add_argument("--render-only", action="store_true")
    return parser


def run_study(argv=None, execute_fn=None) -> int:
    args = build_parser().parse_args(argv)
    if args.render_only:
        doc = json.loads(Path(args.results).read_text())
        Path(args.report).write_text(render_report(doc))
        return 0
    begun = begin_study(args.protocol)
    fn = execute_fn or execute_study
    return fn(begun, args)


def _plain(value):
    if isinstance(value, dict):
        return {str(k): _plain(v) for k, v in value.items()}
    if isinstance(value, (list, tuple)):
        return [_plain(v) for v in value]
    if isinstance(value, np.ndarray):
        return [_plain(v) for v in value.tolist()]
    if isinstance(value, (np.floating, float)):
        number = float(value)
        if not math.isfinite(number):
            return None
        return number
    if isinstance(value, (np.integer,)):
        return int(value)
    if isinstance(value, (np.bool_,)):
        return bool(value)
    return value


def execute_study(begun: dict, args) -> int:
    if not args.snapshot:
        raise SystemExit("a quiescent snapshot path is required")
    print(f"protocol_sha256={begun['sha256']}", flush=True)
    with tempfile.TemporaryDirectory(prefix="regime_1940_") as build_dir:
        return _execute_compiled_study(begun, args, compile_kernel(Path(build_dir)))


def _execute_compiled_study(begun: dict, args, kernel_path: Path) -> int:
    self_check_kernel(Kernel(str(kernel_path)))
    protocol = begun["protocol"]
    expanded = begun["expanded"]
    snap_path = Path(args.snapshot).resolve()
    snapshot_sha = file_sha256(snap_path)
    conn = open_snapshot(snap_path)
    try:
        loaded = _load_all(conn, protocol, expanded)
    finally:
        conn.close()
    args.protocol_sha = begun["sha256"]
    args.snapshot_sha = snapshot_sha
    args.kernel_sha = file_sha256(_THIS.with_name("regime_1940_block_perm.c"))
    scores, populations = _score_all(loaded, protocol, expanded, args, kernel_path)
    apply_bh(scores, expanded["family_size"], float(protocol["alpha"]))
    winners = select_winners(scores, expanded["incumbent_ids"], [row["dataset_id"] for row in expanded["datasets"]])
    suggestions = decide_suggestions(
        winners, scores, float(protocol["alpha"]), protocol["held_out_splits"], protocol["horizons"])
    power = _power(loaded, protocol, expanded, kernel_path, args.workers)
    doc = _assemble(begun, snapshot_sha, snap_path, loaded, scores, populations, winners, suggestions, power)
    results_path = Path(args.results)
    results_path.parent.mkdir(parents=True, exist_ok=True)
    results_path.write_text(json.dumps(_plain(doc), sort_keys=True, separators=(",", ":"), allow_nan=False))
    Path(args.report).write_text(render_report(doc))
    return 0


def _load_all(conn, protocol, expanded) -> dict:
    out = {}
    for ds in expanded["datasets"]:
        for split, bounds in protocol["splits"].items():
            start_ms = parse_time_ms(bounds[0])
            end_ms = parse_time_ms(bounds[1])
            rows = read_candles(conn, ds["exchange"], ds["symbol"], ds["timeframe"], start_ms, end_ms)
            meta = describe_input(rows, ds["exchange"], ds["symbol"], ds["timeframe"], start_ms, end_ms)
            key = f"{ds['dataset_id']}|{split}"
            out[key] = {"meta": meta, "rows": rows if meta["status"] == "available" else [], "dataset": ds, "split": split}
            print(f"load {key} {meta['status']} {meta['reason'] or ''} raw={meta['raw_rows']}", flush=True)
    return out


def _score_all(loaded, protocol, expanded, args, kernel_path) -> tuple[list, dict]:
    import multiprocessing as mp
    scores = []
    populations = {}
    supports = {}
    ctx = mp.get_context("spawn")
    pool = ctx.Pool(args.workers, initializer=_init_worker, initargs=(str(kernel_path),))
    try:
        for ds in expanded["datasets"]:
            for split in protocol["splits"]:
                pack = loaded[f"{ds['dataset_id']}|{split}"]
                cell_rows = [cell for cell in expanded["cells"] if cell["dataset_id"] == ds["dataset_id"] and cell["split"] == split]
                if pack["meta"]["status"] != "available":
                    for cell in cell_rows:
                        populations[cell["cell_id"]] = {**pack["meta"], "cell_id": cell["cell_id"], "usable_rows": 0}
                    continue
                frame = frame_from_rows(pack["rows"])
                built = build_label_bank(frame, expanded["settings"])
                close = built["close"]
                fwds = {int(h): forward_realized_vol(close, int(h)) for h in protocol["horizons"]}
                for cell in cell_rows:
                    horizon = int(cell["horizon"])
                    mask = built["base"] & np.isfinite(fwds[horizon])
                    if not forward_window_inside(mask, horizon):
                        raise RuntimeError(f"forward window leaves the split for {cell['cell_id']}")
                    usable = int(mask.sum())
                    populations[cell["cell_id"]] = {
                        **{k: v for k, v in pack["meta"].items() if k != "status"},
                        "cell_id": cell["cell_id"],
                        "status": "available" if usable >= int(protocol["min_usable_bars"]) else "unavailable",
                        "reason": None if usable >= int(protocol["min_usable_bars"]) else "too_few_usable_bars",
                        "usable_rows": usable,
                        "excluded_rows": int(len(mask) - usable),
                    }
                    if populations[cell["cell_id"]]["status"] != "available":
                        _save_checkpoint(args, cell["cell_id"], [], populations[cell["cell_id"]])
                        continue
                    idx = np.flatnonzero(mask)
                    supports[cell["cell_id"]] = idx
                    saved = _load_checkpoint(args, cell["cell_id"])
                    if saved is not None:
                        scores.extend(saved["scores"])
                        populations[cell["cell_id"]] = saved["population"]
                        print(f"resume {cell['cell_id']}", flush=True)
                        continue
                    runs = support_runs(mask)
                    run_lo = np.array([lo for lo, _hi in runs], dtype=np.int32)
                    run_hi = np.array([hi for _lo, hi in runs], dtype=np.int32)
                    fwd = fwds[horizon][mask]
                    ranks = rank_data(fwd)
                    inv = tie_inv(fwd)
                    stamps = np.array([pack["rows"][i][0] for i in idx], dtype=np.int64)
                    support_sha = hashlib.sha256(stamps.tobytes()).hexdigest()
                    items = []
                    prepared = []
                    for setting in expanded["settings"]:
                        entry = built["bank"][setting["setting_id"]]
                        stats = score_cell_stats(cell, entry, mask, close, int(protocol["block_dwell_multiplier"]))
                        compact = codes_for(entry["labels"][mask], entry["known"])
                        prepared.append((setting, stats, compact))
                    screen_stop = 0
                    full_stop = 40
                    representatives = []
                    member_index = []
                    seen = {}
                    for setting, stats, compact in prepared:
                        key = (int(stats["block_len"]), round(float(stats["kruskal_h"]), 8), compact.tobytes(), bool(stats["degenerate"]))
                        slot = seen.get(key)
                        if slot is None:
                            slot = len(representatives)
                            seen[key] = slot
                            representatives.append((setting, stats, compact))
                        member_index.append(slot)
                    batch = []
                    batches = []
                    for setting, stats, compact in representatives:
                        item = {
                            "labels": compact,
                            "k": stats["k"],
                            "block_len": stats["block_len"],
                            "h_obs": stats["kruskal_h"],
                            "seed": series_seed(protocol["seed"], f"{setting['setting_id']}|{cell['cell_id']}"),
                            "degenerate": stats["degenerate"],
                            "n_perm": int(protocol["n_perm"]),
                        }
                        batch.append(item)
                        if len(batch) == 20:
                            batches.append(batch)
                            batch = []
                    if batch:
                        batches.append(batch)
                    tasks = [{
                        "ranks": ranks, "run_lo": run_lo, "run_hi": run_hi, "inv_tie": inv,
                        "screen_n": int(protocol["screen_n"]), "screen_stop": screen_stop,
                        "screen_max_p": float(protocol["screen_continue_if_p_at_most"]),
                        "n_perm": int(protocol["n_perm"]), "full_stop": full_stop,
                        "allow_full": split == "is" and int(cell["horizon"]) == 4, "items": one,
                    } for one in batches]
                    perm_rows = []
                    for chunk in pool.map(_perm_task, tasks):
                        perm_rows.extend(chunk)
                    if len(perm_rows) != len(representatives):
                        raise RuntimeError("permutation batch length mismatch")
                    for (setting, stats, _compact), rep in zip(prepared, member_index):
                        perm = perm_rows[rep]
                        _validate_kernel_h(perm, stats, setting["setting_id"], cell["cell_id"])
                        scores.append({
                            "status": "available",
                            "setting_id": setting["setting_id"],
                            "classifier": setting["classifier"],
                            "dataset_id": ds["dataset_id"],
                            "split": split,
                            "horizon": horizon,
                            "cell_id": cell["cell_id"],
                            "support_sha256": support_sha,
                            "support_rows": usable,
                            "kruskal_h": stats["kruskal_h"],
                            "p_value": float(perm["p_value"]),
                            "p_kind": perm["kind"],
                            "ge": int(perm["ge"]),
                            "n_perm_executed": int(perm["executed"]),
                            "transition_rate": stats["transition_rate"],
                            "flips": stats["flips"],
                            "mean_dwell": stats["mean_dwell"],
                            "coverage": stats["coverage"],
                            "mean_vol": stats["mean_vol"],
                            "median_vol": stats["median_vol"],
                            "order_by_mean": stats["order_by_mean"],
                            "order_by_median": stats["order_by_median"],
                            "block_len": stats["block_len"],
                        })
                    _save_checkpoint(args, cell["cell_id"], scores[-len(prepared):], populations[cell["cell_id"]])
                    print(f"scored {cell['cell_id']} rows={usable} settings={len(prepared)} unique={len(representatives)}", flush=True)
        winners = select_winners(scores, expanded["incumbent_ids"], [row["dataset_id"] for row in expanded["datasets"]])
        _refine_confirmation(pool, loaded, scores, winners, protocol, expanded, supports)
    finally:
        pool.close()
        pool.join()
    return scores, populations


def _refine_confirmation(pool, loaded, scores, winners, protocol, expanded, supports) -> None:
    wanted = {}
    for winner in winners:
        ids = {winner["incumbent_id"]}
        if winner.get("setting_id"):
            ids.add(winner["setting_id"])
        wanted.setdefault(winner["dataset_id"], set()).update(ids)
    index = {(row["dataset_id"], row["split"], int(row["horizon"]), row["setting_id"]): row for row in scores}
    for ds in expanded["datasets"]:
        ids = wanted.get(ds["dataset_id"]) or set()
        if not ids:
            continue
        settings = [row for row in expanded["settings"] if row["setting_id"] in ids]
        for split in protocol["held_out_splits"]:
            pack = loaded.get(f"{ds['dataset_id']}|{split}")
            if pack is None or pack["meta"]["status"] != "available":
                continue
            frame = frame_from_rows(pack["rows"])
            built = build_label_bank(frame, settings)
            close = built["close"]
            for horizon in protocol["horizons"]:
                sample = next((row for row in scores if row["dataset_id"] == ds["dataset_id"] and row["split"] == split and int(row["horizon"]) == int(horizon)), None)
                if sample is None:
                    continue
                idx = supports.get(sample["cell_id"])
                if idx is None or len(idx) < int(protocol["min_usable_bars"]):
                    continue
                mask = np.zeros(len(frame), dtype=bool)
                mask[idx] = True
                runs = support_runs(mask)
                run_lo = np.array([lo for lo, _hi in runs], dtype=np.int32)
                run_hi = np.array([hi for _lo, hi in runs], dtype=np.int32)
                fwd = forward_realized_vol(close, int(horizon))[mask]
                ranks = rank_data(fwd)
                items = []
                targets = []
                for setting in settings:
                    row = index.get((ds["dataset_id"], split, int(horizon), setting["setting_id"]))
                    if row is None or row.get("p_kind") == "full":
                        continue
                    entry = built["bank"][setting["setting_id"]]
                    stats = score_cell_stats({"horizon": int(horizon)}, entry, mask, close, int(protocol["block_dwell_multiplier"]))
                    items.append({
                        "labels": codes_for(entry["labels"][mask], entry["known"]),
                        "k": stats["k"], "block_len": stats["block_len"], "h_obs": stats["kruskal_h"],
                        "seed": series_seed(protocol["seed"], f"{setting['setting_id']}|{row['cell_id']}"),
                        "degenerate": stats["degenerate"], "n_perm": int(protocol["n_perm"]),
                    })
                    targets.append((row, stats))
                if not items:
                    continue
                task = {
                    "ranks": ranks, "run_lo": run_lo, "run_hi": run_hi, "inv_tie": tie_inv(fwd),
                    "screen_n": int(protocol["screen_n"]), "screen_stop": 0,
                    "screen_max_p": float(protocol["screen_continue_if_p_at_most"]),
                    "n_perm": int(protocol["n_perm"]), "full_stop": 40, "allow_full": True, "items": items,
                }
                for (row, stats), perm in zip(targets, pool.map(_perm_task, [task])[0]):
                    row["kruskal_h"] = stats["kruskal_h"]
                    row["p_value"] = float(perm["p_value"])
                    row["p_kind"] = perm["kind"]
                    row["ge"] = int(perm["ge"])
                    row["n_perm_executed"] = int(perm["executed"])
                    row["block_len"] = stats["block_len"]
                print(f"refined {ds['dataset_id']}|{split}|h{horizon} n={len(items)}", flush=True)


def _power(loaded, protocol, expanded, kernel_path, workers: int) -> list[dict]:
    import multiprocessing as mp
    mod = _load_1211()
    out = []
    ctx = mp.get_context("spawn")
    pool = ctx.Pool(workers, initializer=_init_power_worker, initargs=(str(kernel_path),))
    try:
        return _power_curves(pool, loaded, protocol, expanded, mod)
    finally:
        pool.close()
        pool.join()


def _power_curves(pool, loaded, protocol, expanded, mod) -> list[dict]:
    out = []
    alpha = float(protocol["alpha"])
    family = expanded["family_size"]
    n_perm = int(protocol["n_perm"])
    for ds in expanded["datasets"]:
        pack = loaded.get(f"{ds['dataset_id']}|is")
        for classifier, incumbent in expanded["incumbent_ids"].items():
            for horizon in protocol["horizons"]:
                key = {"dataset_id": ds["dataset_id"], "classifier": classifier, "horizon": int(horizon)}
                if pack is None or pack["meta"]["status"] != "available":
                    out.append({**key, "status": "unavailable", "reason": "training candles unavailable"})
                    continue
                frame = frame_from_rows(pack["rows"])
                setting = next(row for row in expanded["settings"] if row["setting_id"] == incumbent)
                built = build_label_bank(frame, [setting])
                close = built["close"]
                fwd_full = forward_realized_vol(close, int(horizon))
                mask = built["base"] & np.isfinite(fwd_full)
                if int(mask.sum()) < int(protocol["min_usable_bars"]):
                    out.append({**key, "status": "unavailable", "reason": "too_few_usable_bars"})
                    continue
                labels = built["bank"][incumbent]["labels"][mask]
                fwd = fwd_full[mask]
                profile = mod.forward_vol_profile(labels, fwd)
                if not profile:
                    out.append({**key, "status": "unavailable", "reason": "no training profile"})
                    continue
                stab = gapped_stability(built["bank"][incumbent]["labels"], mask)
                blen = block_length(stab, horizon, int(protocol["block_dwell_multiplier"]))
                known = built["bank"][incumbent]["known"]
                powers = {}
                for lam in protocol["power_lam_grid"]:
                    tasks = []
                    for sim in range(int(protocol["power_n_sim"])):
                        tasks.append({
                            "seed": series_seed(protocol["seed"], f"power|{ds['dataset_id']}|{classifier}|h{horizon}|{lam}|{sim}"),
                            "fwd": fwd, "labels": labels, "profile": profile, "lam": float(lam),
                            "block_len": blen, "known": list(known), "n_perm": n_perm,
                            "alpha": alpha, "family": family,
                        })
                    hits = int(sum(pool.map(_power_sim, tasks)))
                    powers[float(lam)] = hits / float(protocol["power_n_sim"])
                    print(f"power {ds['dataset_id']} {classifier} h{horizon} lam={lam} power={powers[float(lam)]}", flush=True)
                limit = detection_limit(protocol["power_lam_grid"], powers, labels, fwd, profile, float(protocol["power_target"]))
                out.append({**key, **limit})
    return out


def _assemble(begun, snapshot_sha, snap_path, loaded, scores, populations, winners, suggestions, power) -> dict:
    protocol = begun["protocol"]
    expanded = begun["expanded"]
    inputs = []
    for cell in expanded["cells"]:
        pop = populations.get(cell["cell_id"])
        if pop is None:
            ds, split, _horizon = cell["dataset_id"], cell["split"], cell["horizon"]
            meta = loaded[f"{ds}|{split}"]["meta"]
            pop = {**meta, "cell_id": cell["cell_id"], "usable_rows": 0, "status": "unavailable"}
        inputs.append(pop)
    limit_by = {(row["dataset_id"], row["classifier"], int(row["horizon"])): row for row in power}
    highlights = []
    nulls = []
    for row in scores:
        limit = limit_by.get((row["dataset_id"], row["classifier"], int(row["horizon"])))
        brief = _limit_brief(limit)
        view = {**row, "detection": brief}
        if row["classifier"] == "adx" and row["setting_id"] == expanded["incumbent_ids"]["adx"]:
            highlights.append(view)
        if not row.get("corrected_pass"):
            nulls.append({
                "setting_id": row["setting_id"],
                "cell_id": row["cell_id"],
                "kruskal_h": row["kruskal_h"],
                "p_value": row["p_value"],
                "detection": brief,
            })
    order_highlights = []
    for row in scores:
        if row["setting_id"] in expanded["incumbent_ids"].values() and row["split"] == "is":
            quiet = row["mean_vol"].get("ranging_quiet")
            volatile = row["mean_vol"].get("ranging_volatile")
            order_highlights.append({
                "cell_id": row["cell_id"],
                "setting_id": row["setting_id"],
                "order_by_mean": row["order_by_mean"],
                "order_by_median": row["order_by_median"],
                "quiet_below_volatile": None if quiet is None or volatile is None else quiet < volatile,
            })
    return {
        "protocol_sha256": begun["sha256"],
        "family_size": expanded["family_size"],
        "alpha": protocol["alpha"],
        "n_perm": protocol["n_perm"],
        "screen_n": protocol["screen_n"],
        "screen_continue_if_p_at_most": protocol["screen_continue_if_p_at_most"],
        "floor_n_perm": expanded["floor_n_perm"],
        "headroom_n_perm": expanded["headroom_n_perm"],
        "setting_ids": expanded["setting_ids"],
        "cell_ids": expanded["cell_ids"],
        "incumbent_ids": expanded["incumbent_ids"],
        "snapshot_sha256": snapshot_sha,
        "snapshot_bytes": snap_path.stat().st_size,
        "code_revision": subprocess.check_output(["git", "rev-parse", "HEAD"], cwd=_ROOT, text=True).strip(),
        "study_script_sha256": hashlib.sha256(_THIS.read_bytes()).hexdigest(),
        "inputs": inputs,
        "scores": scores,
        "selection": winners,
        "suggestions": suggestions,
        "power": power,
        "adx_highlights": highlights,
        "nulls": nulls,
        "order_highlights": order_highlights,
        "comparisons": _comparisons(scores, expanded),
        "confusion": _confusion(loaded, expanded, protocol),
        "cap_agreement": _cap_agreement(loaded, expanded, protocol),
        "sensitivity": _sensitivity(scores, expanded),
        "asset_notes": _asset_notes(suggestions),
        "dwell_highlights": [row for row in scores if row["setting_id"] in expanded["incumbent_ids"].values() and row["split"] == "is"],
        "suggest_only": True,
    }


def _comparisons(scores, expanded) -> list[dict]:
    out = []
    by = {(row["dataset_id"], row["split"], row["setting_id"], int(row["horizon"])): row for row in scores}
    for ds in expanded["datasets"]:
        for split in ("is", "oos", "2023", "2024", "2025H1"):
            adx = by.get((ds["dataset_id"], split, expanded["incumbent_ids"]["adx"], 4))
            comp = by.get((ds["dataset_id"], split, expanded["incumbent_ids"]["composite"], 4))
            if adx is None or comp is None:
                text = "unavailable"
            else:
                text = f"ADX H={adx['kruskal_h']:.6g} composite H={comp['kruskal_h']:.6g}"
            out.append({"dataset_id": ds["dataset_id"], "split": split, "text": text})
    return out


def _confusion(loaded, expanded, protocol) -> list[dict]:
    out = []
    for ds in expanded["datasets"]:
        pack = loaded.get(f"{ds['dataset_id']}|is")
        if pack is None or pack["meta"]["status"] != "available":
            out.append({"name": f"{ds['dataset_id']} defaults", "counts": {}})
            continue
        frame = frame_from_rows(pack["rows"])
        ids = [expanded["incumbent_ids"]["adx"], expanded["incumbent_ids"]["composite"]]
        settings = [row for row in expanded["settings"] if row["setting_id"] in ids]
        built = build_label_bank(frame, settings)
        fwd = forward_realized_vol(built["close"], 4)
        mask = built["base"] & np.isfinite(fwd)
        adx = built["bank"][ids[0]]["labels"][mask]
        comp = built["bank"][ids[1]]["labels"][mask]
        counts = {}
        for a, c in zip(adx, comp):
            key = f"{a}|{c}"
            counts[key] = counts.get(key, 0) + 1
        out.append({"name": f"{ds['dataset_id']} defaults h4", "counts": counts})
    return out


def _cap_agreement(loaded, expanded, protocol) -> list[dict]:
    out = []
    periods = protocol["periods"]
    for ds in expanded["datasets"]:
        for split in protocol["splits"]:
            pack = loaded.get(f"{ds['dataset_id']}|{split}")
            if pack is None or pack["meta"]["status"] != "available":
                continue
            frame = frame_from_rows(pack["rows"])
            pair_ids = []
            for period in periods:
                pair_ids.append(setting_id_composite(period, 0.05, 0.03, 0.5, 25, 14))
                pair_ids.append(setting_id_composite(period, 0.05, 0.03, 0.5, 25, None))
            settings = [row for row in expanded["settings"] if row["setting_id"] in pair_ids]
            built = build_label_bank(frame, settings)
            for period in periods:
                capped = setting_id_composite(period, 0.05, 0.03, 0.5, 25, 14)
                free = setting_id_composite(period, 0.05, 0.03, 0.5, 25, None)
                mask = built["base"]
                if int(mask.sum()) == 0:
                    continue
                a = built["bank"][capped]["labels"][mask]
                b = built["bank"][free]["labels"][mask]
                out.append({
                    "dataset_id": ds["dataset_id"],
                    "split": split,
                    "pair": f"period={period}",
                    "agreement": float(np.mean(a == b)),
                })
    return out


def _sensitivity(scores, expanded) -> list[str]:
    notes = []
    incumb = expanded["incumbent_ids"]["composite"]
    is_rows = [row for row in scores if row["split"] == "is" and int(row["horizon"]) == 4 and row["classifier"] == "composite"]
    by_ds = {}
    for row in is_rows:
        by_ds.setdefault(row["dataset_id"], []).append(row)
    for ds, rows in sorted(by_ds.items()):
        best = max(rows, key=lambda row: (row["kruskal_h"], -row["transition_rate"], row["setting_id"]))
        base = next((row for row in rows if row["setting_id"] == incumb), None)
        if base is None:
            notes.append(f"{ds}: composite incumbent unavailable on is h4")
        else:
            notes.append(
                f"{ds}: composite incumbent H={base['kruskal_h']:.6g}; highest is h4 H={best['kruskal_h']:.6g} at {best['setting_id']}"
            )
    return notes


def _asset_notes(suggestions: list[dict]) -> list[str]:
    notes = []
    for row in suggestions:
        notes.append(f"{row['dataset_id']} {row['classifier']}: {row['reason']}")
    return notes or ["no dataset comparison was available"]


def main(argv=None) -> int:
    return run_study(argv)


if __name__ == "__main__":
    raise SystemExit(main())

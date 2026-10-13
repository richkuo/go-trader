import math
import os
import sys
from numbers import Real
from statistics import NormalDist

import numpy as np
import pandas as pd

_TOOLS_DIR = os.path.abspath(os.path.join(os.path.dirname(os.path.abspath(__file__)), "..", "..", "shared_tools"))
if _TOOLS_DIR not in sys.path:
    sys.path.append(_TOOLS_DIR)

from closed_bar import (
    FIXED_DURATION_MS,
    FRAME_DECISION_TIME_ATTR,
    FRAME_INTERVAL_ATTR,
    FRAME_RECEIVED_ORDER_DEFECTS_ATTR,
    FRAME_TIMESTAMP_KIND_ATTR,
    TIMESTAMP_KIND_NATIVE_CLOSE,
    TIMESTAMP_KIND_OPEN,
)

NAME = "calendar_conditioned_entry"
HOUR_MS = 3_600_000
DAY_MS = 86_400_000
WEEK_MS = 7 * DAY_MS
EPOCH_MS = 345_600_000
CELLS = 7 * 24
DIRECTIONS = 2
FAMILY_SIZE = CELLS * DIRECTIONS
SUPPORTED_INTERVALS_MS = {ms: tf for tf, ms in FIXED_DURATION_MS.items()}

DEFAULT_PARAMS = {
    "training_bars": 4368,
    "holding_bars": 4,
    "refit_bars": 168,
    "min_effective_obs": 8,
    "confidence": 0.9,
    "cost_hurdle": 0.002,
    "expected_interval_ms": HOUR_MS,
}
REQUIRED_COLUMNS = ("open", "high", "low", "close")

REASON_CODES = {
    "entry_long": 1,
    "entry_short": -1,
    "below_threshold": 0,
    "direction_tie": 2,
    "empty_cell": 3,
    "insufficient_effective_obs": 4,
    "no_uncertainty_estimate": 5,
    "nonfinite_statistic": 6,
    "insufficient_history": 10,
    "missing_timestamp": 11,
    "duplicate_timestamp": 12,
    "descending_timestamp": 13,
    "received_out_of_order": 14,
    "cadence_gap": 15,
    "grid_misaligned": 16,
    "nonfinite_price": 17,
    "nonpositive_price": 18,
    "inconsistent_candle": 19,
    "history_missing_timestamp": 21,
    "history_duplicate_timestamp": 22,
    "history_descending_timestamp": 23,
    "history_received_out_of_order": 24,
    "history_cadence_gap": 25,
    "history_grid_misaligned": 26,
    "history_nonfinite_price": 27,
    "history_nonpositive_price": 28,
    "history_inconsistent_candle": 29,
    "missing_columns": 40,
    "missing_timestamps": 41,
    "undeclared_timing": 42,
    "timing_contradiction": 43,
    "decision_time_missing": 44,
    "decision_bar_stale": 45,
    "decision_bar_future": 46,
    "decision_boundary_mismatch": 47,
}
CELL_REASONS = ("below_threshold", "entry_long", "entry_short", "direction_tie", "empty_cell",
                "insufficient_effective_obs", "no_uncertainty_estimate", "nonfinite_statistic")
ROW_REASONS = ("", "missing_timestamp", "grid_misaligned", "received_out_of_order", "nonfinite_price",
               "nonpositive_price", "inconsistent_candle")
EDGE_REASONS = ("", "missing_timestamp", "duplicate_timestamp", "descending_timestamp", "cadence_gap")

VALUE_COLUMNS = (
    "cce_eval_ts_ms", "cce_next_open_ms", "cce_next_weekday", "cce_next_hour", "cce_next_cell",
    "cce_refit_boundary_ms", "cce_refit_bar_open_ms", "cce_training_first_open_ms", "cce_history_bars",
    "cce_required_bars", "cce_gross_mean", "cce_long_net_edge", "cce_short_net_edge", "cce_long_lower_bound",
    "cce_short_lower_bound", "cce_raw_obs", "cce_weekly_obs", "cce_effective_obs", "cce_std_error",
    "cce_nw_lag", "cce_family_size", "cce_corrected_z", "cce_corrected_alpha", "cce_cell_decision",
    "cce_decision_cutoff_ms", "cce_decision_boundary_ms",
)


def _integer(name: str, value, minimum: int) -> int:
    if isinstance(value, (bool, np.bool_)) or not isinstance(value, Real):
        raise ValueError(f"{NAME}: {name} must be an integer, got {value!r}")
    as_float = float(value)
    if not math.isfinite(as_float) or not as_float.is_integer():
        raise ValueError(f"{NAME}: {name} must be an integer, got {value!r}")
    as_int = int(as_float)
    if as_int < minimum:
        raise ValueError(f"{NAME}: {name} must be >= {minimum}, got {value!r}")
    return as_int


def _real(name: str, value) -> float:
    if isinstance(value, (bool, np.bool_)) or not isinstance(value, Real):
        raise ValueError(f"{NAME}: {name} must be a finite number, got {value!r}")
    as_float = float(value)
    if not math.isfinite(as_float):
        raise ValueError(f"{NAME}: {name} must be a finite number, got {value!r}")
    return as_float


def validate_calendar_params(params: dict) -> dict:
    merged = {**DEFAULT_PARAMS, **(params or {})}
    out = {
        "training_bars": _integer("training_bars", merged["training_bars"], 2),
        "holding_bars": _integer("holding_bars", merged["holding_bars"], 1),
        "refit_bars": _integer("refit_bars", merged["refit_bars"], 1),
        "min_effective_obs": _integer("min_effective_obs", merged["min_effective_obs"], 2),
        "confidence": _real("confidence", merged["confidence"]),
        "cost_hurdle": _real("cost_hurdle", merged["cost_hurdle"]),
        "expected_interval_ms": _integer("expected_interval_ms", merged["expected_interval_ms"], 1),
    }
    if not 0.0 < out["confidence"] < 1.0:
        raise ValueError(f"{NAME}: confidence must be strictly between 0 and 1, got {merged['confidence']!r}")
    if out["cost_hurdle"] < 0.0:
        raise ValueError(f"{NAME}: cost_hurdle must be >= 0, got {merged['cost_hurdle']!r}")
    if out["holding_bars"] >= out["training_bars"]:
        raise ValueError(f"{NAME}: holding_bars ({out['holding_bars']}) must be smaller than training_bars "
                         f"({out['training_bars']}) so a completed label fits the training sample")
    if out["refit_bars"] > out["training_bars"]:
        raise ValueError(f"{NAME}: refit_bars ({out['refit_bars']}) must not exceed training_bars "
                         f"({out['training_bars']})")
    if out["expected_interval_ms"] not in SUPPORTED_INTERVALS_MS:
        supported = ", ".join(f"{ms} ({tf})" for ms, tf in sorted(SUPPORTED_INTERVALS_MS.items()))
        raise ValueError(f"{NAME}: expected_interval_ms={merged['expected_interval_ms']!r} is not a supported "
                         f"interval; supported: {supported}")
    return out


def calendar_param_validator(params: dict) -> dict:
    return validate_calendar_params(params)


def required_history_bars(params: dict) -> int:
    p = validate_calendar_params(params)
    return p["training_bars"] + p["refit_bars"] - 1


def corrected_threshold(confidence: float) -> tuple:
    alpha = (1.0 - float(confidence)) / FAMILY_SIZE
    return alpha, NormalDist().inv_cdf(1.0 - alpha)


def newey_west_lag(holding_bars: int, interval_ms: int) -> int:
    return max(1, int(math.ceil(holding_bars * interval_ms / WEEK_MS)))


def calendar_cell(open_ms):
    rel = np.asarray(open_ms, dtype=np.int64) - EPOCH_MS
    weekday = (rel // DAY_MS) % 7
    hour = (rel % DAY_MS) // HOUR_MS
    return weekday * 24 + hour


def calendar_week(open_ms):
    return (np.asarray(open_ms, dtype=np.int64) - EPOCH_MS) // WEEK_MS


def refit_boundary_ms(decision_open_ms: int, interval_ms: int, refit_bars: int) -> int:
    boundary = int(decision_open_ms) + int(interval_ms)
    grid = int(refit_bars) * int(interval_ms)
    return EPOCH_MS + ((boundary - EPOCH_MS) // grid) * grid


def _absolute_ms(df: pd.DataFrame):
    if isinstance(df.index, pd.DatetimeIndex):
        idx = df.index
        if idx.tz is not None:
            idx = idx.tz_convert("UTC").tz_localize(None)
        ns_per_unit = {"s": 1_000_000_000, "ms": 1_000_000, "us": 1_000, "ns": 1}[idx.unit]
        missing = np.asarray(idx.isna())
        raw = np.where(missing, 0, idx.asi8)
        values = ((raw * ns_per_unit) // 1_000_000).astype("float64")
        values[missing] = np.nan
        return values
    if "timestamp" in df.columns:
        return pd.to_numeric(df["timestamp"], errors="coerce").to_numpy(dtype="float64")
    return None


def _frame_timing(df: pd.DataFrame, interval: int):
    declared = df.attrs.get(FRAME_INTERVAL_ATTR)
    kind = df.attrs.get(FRAME_TIMESTAMP_KIND_ATTR)
    if declared is None or kind is None:
        return None, "undeclared_timing"
    if (isinstance(declared, (bool, np.bool_)) or not isinstance(declared, (int, np.integer))
            or int(declared) != interval):
        return None, "timing_contradiction"
    if kind not in (TIMESTAMP_KIND_OPEN, TIMESTAMP_KIND_NATIVE_CLOSE):
        return None, "timing_contradiction"
    ms = _absolute_ms(df)
    if ms is None:
        return None, "missing_timestamps"
    open_ms = ms.copy()
    if kind == TIMESTAMP_KIND_NATIVE_CLOSE:
        open_ms = ms - float(interval) + 1.0
    return open_ms, None


def _decision_time(attrs: dict):
    if FRAME_DECISION_TIME_ATTR not in attrs:
        return None, None, False
    raw = attrs.get(FRAME_DECISION_TIME_ATTR)
    cutoff = boundary = None
    if isinstance(raw, dict):
        for key in ("cutoff_ms", "boundary_ms"):
            value = raw.get(key)
            if isinstance(value, (int, np.integer)) and not isinstance(value, (bool, np.bool_)) and int(value) > 0:
                if key == "cutoff_ms":
                    cutoff = int(value)
                else:
                    boundary = int(value)
    return cutoff, boundary, True


class _Frame:
    def __init__(self, df: pd.DataFrame, interval: int, open_ms: np.ndarray):
        n = len(df)
        self.n = n
        self.interval = interval
        o = pd.to_numeric(df["open"], errors="coerce").to_numpy(dtype="float64")
        h = pd.to_numeric(df["high"], errors="coerce").to_numpy(dtype="float64")
        lo = pd.to_numeric(df["low"], errors="coerce").to_numpy(dtype="float64")
        c = pd.to_numeric(df["close"], errors="coerce").to_numpy(dtype="float64")
        self.open = o
        ts_ok = np.isfinite(open_ms)
        aligned = np.zeros(n, dtype=bool)
        aligned[ts_ok] = np.mod(open_ms[ts_ok], float(interval)) == 0.0
        received = df.attrs.get(FRAME_RECEIVED_ORDER_DEFECTS_ATTR) or ()
        raw_ms = _absolute_ms(df)
        flagged = np.zeros(n, dtype=bool)
        if len(received) and raw_ms is not None:
            flagged = np.isin(raw_ms, np.asarray(list(received), dtype="float64"))
        finite = np.isfinite(o) & np.isfinite(h) & np.isfinite(lo) & np.isfinite(c)
        with np.errstate(invalid="ignore"):
            positive = finite & (o > 0.0) & (h > 0.0) & (lo > 0.0) & (c > 0.0)
            consistent = positive & (h >= lo) & (h >= o) & (h >= c) & (lo <= o) & (lo <= c)
        row_reason = np.zeros(n, dtype=np.int64)
        row_reason[positive & ~consistent] = ROW_REASONS.index("inconsistent_candle")
        row_reason[finite & ~positive] = ROW_REASONS.index("nonpositive_price")
        row_reason[~finite] = ROW_REASONS.index("nonfinite_price")
        row_reason[flagged] = ROW_REASONS.index("received_out_of_order")
        row_reason[ts_ok & ~aligned] = ROW_REASONS.index("grid_misaligned")
        row_reason[~ts_ok] = ROW_REASONS.index("missing_timestamp")
        self.row_reason = row_reason
        self.row_bad = row_reason > 0
        edge_reason = np.zeros(n, dtype=np.int64)
        if n > 1:
            both = ts_ok[1:] & ts_ok[:-1]
            with np.errstate(invalid="ignore"):
                step = open_ms[1:] - open_ms[:-1]
            er = np.zeros(n - 1, dtype=np.int64)
            er[both & (step != float(interval))] = EDGE_REASONS.index("cadence_gap")
            er[both & (step < 0)] = EDGE_REASONS.index("descending_timestamp")
            er[both & (step == 0)] = EDGE_REASONS.index("duplicate_timestamp")
            er[~both] = EDGE_REASONS.index("missing_timestamp")
            edge_reason[1:] = er
        self.edge_reason = edge_reason
        brk = self.row_bad | (edge_reason > 0)
        brk[0] = True
        pos = np.where(brk, np.arange(n), -1)
        last = np.maximum.accumulate(pos) if n else pos
        self.break_index = last
        self.run_start = np.where(self.row_bad[last], last + 1, last)
        self.open_ms = np.where(ts_ok, open_ms, 0.0).astype(np.int64)
        self.cell = calendar_cell(self.open_ms)
        self.week = calendar_week(self.open_ms)

    def history_reasons(self, rows: np.ndarray) -> np.ndarray:
        row_names = np.array(ROW_REASONS, dtype=object)
        edge_names = np.array(EDGE_REASONS, dtype=object)
        hist_row = np.array(["history_" + r for r in ROW_REASONS], dtype=object)
        hist_edge = np.array(["history_" + r for r in EDGE_REASONS], dtype=object)
        k = self.break_index[rows]
        row_bad_i = self.row_bad[rows]
        row_bad_k = self.row_bad[k]
        edge_k = self.edge_reason[k]
        conditions = [row_bad_i, (k == 0) & ~row_bad_k, (k == rows) & (edge_k > 0), row_bad_k]
        choices = [row_names[self.row_reason[rows]], np.full(len(rows), "insufficient_history", dtype=object),
                   edge_names[edge_k], hist_row[self.row_reason[k]]]
        return np.select(conditions, choices, default=hist_edge[edge_k])


def _fit_table(frame: _Frame, r: int, p: dict, z: float, lag: int) -> dict:
    T, H = p["training_bars"], p["holding_bars"]
    first = r - T + 1
    entries = np.arange(first, r - H + 1)
    o = frame.open
    labels = o[entries + H] / o[entries] - 1.0
    cells = frame.cell[entries]
    weeks = frame.week[entries]
    w0 = int(weeks.min())
    nweeks = int(weeks.max()) - w0 + 1
    key = (weeks - w0) * CELLS + cells
    sums = np.bincount(key, weights=labels, minlength=nweeks * CELLS).reshape(nweeks, CELLS)
    counts = np.bincount(key, minlength=nweeks * CELLS).reshape(nweeks, CELLS)
    with np.errstate(invalid="ignore", divide="ignore"):
        cluster = np.where(counts > 0, sums / np.where(counts > 0, counts, 1), np.nan)
    present = counts > 0
    n = present.sum(axis=0).astype(np.int64)
    raw = counts.sum(axis=0).astype(np.int64)
    with np.errstate(invalid="ignore", divide="ignore"):
        mean = np.where(n > 0, np.nansum(cluster, axis=0) / np.where(n > 0, n, 1), np.nan)
        dev = cluster - mean
        gamma0 = np.where(n > 0, np.nansum(dev * dev, axis=0) / np.where(n > 0, n, 1), np.nan)
        s = gamma0.copy()
        for lag_i in range(1, lag + 1):
            if lag_i >= nweeks:
                break
            prod = dev[lag_i:] * dev[:-lag_i]
            gamma = np.nansum(prod, axis=0) / np.where(n > 0, n, 1)
            s = s + 2.0 * (1.0 - lag_i / (lag + 1.0)) * gamma
        var_used = np.maximum(s, gamma0)
        n_eff = np.where(var_used > 0, n * gamma0 / var_used, 0.0)
        small = np.where(n > 1, n / np.maximum(n - 1, 1), np.nan)
        se = np.sqrt(var_used * small / np.where(n > 0, n, 1))
        long_net = mean - p["cost_hurdle"]
        short_net = -mean - p["cost_hurdle"]
        long_lb = long_net - z * se
        short_lb = short_net - z * se
    reason = np.full(CELLS, CELL_REASONS.index("below_threshold"), dtype=np.int64)
    decision = np.zeros(CELLS, dtype=np.int64)
    long_ok = long_lb > 0.0
    short_ok = short_lb > 0.0
    finite = np.isfinite(long_lb) & np.isfinite(short_lb) & np.isfinite(se) & np.isfinite(n_eff)
    decision[long_ok & ~short_ok] = 1
    decision[short_ok & ~long_ok] = -1
    both = long_ok & short_ok
    decision[both & (long_lb > short_lb)] = 1
    decision[both & (short_lb > long_lb)] = -1
    reason[decision == 1] = CELL_REASONS.index("entry_long")
    reason[decision == -1] = CELL_REASONS.index("entry_short")
    reason[both & (long_lb == short_lb)] = CELL_REASONS.index("direction_tie")
    eff_short = n_eff < p["min_effective_obs"]
    zero_se = ~(se > 0.0)
    not_finite = ~finite & ~zero_se
    for mask, name in ((eff_short, "insufficient_effective_obs"), (not_finite, "nonfinite_statistic"),
                       (zero_se, "no_uncertainty_estimate"), (n == 0, "empty_cell")):
        decision[mask] = 0
        reason[mask] = CELL_REASONS.index(name)
    return {
        "refit_index": r,
        "refit_bar_open_ms": int(frame.open_ms[r]),
        "refit_boundary_ms": int(frame.open_ms[r]) + frame.interval,
        "training_first_open_ms": int(frame.open_ms[first]),
        "labels": int(len(entries)),
        "last_label_exit_open_ms": int(frame.open_ms[r]),
        "nweeks": nweeks,
        "n": n, "raw": raw, "mean": mean, "se": se, "n_eff": n_eff, "long_net": long_net,
        "short_net": short_net, "long_lb": long_lb, "short_lb": short_lb, "decision": decision, "reason": reason,
    }


def _hold_all(result: pd.DataFrame, p: dict, reason: str) -> pd.DataFrame:
    n = len(result)
    for col in VALUE_COLUMNS:
        result[col] = np.full(n, np.nan)
    result["cce_interval_ms"] = np.full(n, float(p["expected_interval_ms"]))
    result["cce_valid"] = np.zeros(n, dtype="float64")
    result["cce_reason"] = np.full(n, reason, dtype=object)
    result["cce_reason_code"] = np.full(n, float(REASON_CODES[reason]))
    result["signal"] = np.zeros(n, dtype=np.int64)
    return result


def _prepare(df: pd.DataFrame, p: dict):
    if any(c not in df.columns for c in REQUIRED_COLUMNS):
        return None, "missing_columns"
    open_ms, frame_reason = _frame_timing(df, p["expected_interval_ms"])
    if frame_reason:
        return None, frame_reason
    if len(df) == 0:
        return None, "insufficient_history"
    return _Frame(df, p["expected_interval_ms"], open_ms), None


def calendar_conditioned_entry_core(df: pd.DataFrame, **params) -> pd.DataFrame:
    p = validate_calendar_params(params)
    result = df.copy()
    frame, reason = _prepare(result, p)
    if reason:
        return _hold_all(result, p, reason)
    n = frame.n
    interval = p["expected_interval_ms"]
    alpha, z = corrected_threshold(p["confidence"])
    lag = newey_west_lag(p["holding_bars"], interval)
    T = p["training_bars"]

    cols = {c: np.full(n, np.nan) for c in VALUE_COLUMNS}
    signal = np.zeros(n, dtype=np.int64)
    valid = np.zeros(n, dtype=bool)
    reasons = np.empty(n, dtype=object)

    cols["cce_eval_ts_ms"] = np.where(frame.row_reason == ROW_REASONS.index("missing_timestamp"), np.nan,
                                      frame.open_ms.astype("float64"))
    next_open = frame.open_ms + interval
    next_cell = calendar_cell(next_open)
    ok_ts = np.isfinite(cols["cce_eval_ts_ms"])
    cols["cce_next_open_ms"] = np.where(ok_ts, next_open.astype("float64"), np.nan)
    cols["cce_next_cell"] = np.where(ok_ts, next_cell.astype("float64"), np.nan)
    cols["cce_next_weekday"] = np.where(ok_ts, (next_cell // 24).astype("float64"), np.nan)
    cols["cce_next_hour"] = np.where(ok_ts, (next_cell % 24).astype("float64"), np.nan)
    cols["cce_family_size"] = np.full(n, float(FAMILY_SIZE))
    cols["cce_corrected_z"] = np.full(n, z)
    cols["cce_corrected_alpha"] = np.full(n, alpha)
    cols["cce_nw_lag"] = np.full(n, float(lag))

    idx = np.arange(n)
    row_ok = ~frame.row_bad
    boundary = frame.open_ms + interval
    grid = p["refit_bars"] * interval
    refit_boundary = EPOCH_MS + ((boundary - EPOCH_MS) // grid) * grid
    d = (boundary - refit_boundary) // interval
    refit_row = idx - d
    available = idx - frame.run_start + 1
    need = T + d
    cols["cce_history_bars"] = np.where(row_ok, available.astype("float64"), np.nan)
    cols["cce_required_bars"] = np.where(row_ok, need.astype("float64"), np.nan)
    cols["cce_refit_boundary_ms"] = np.where(row_ok, refit_boundary.astype("float64"), np.nan)
    enough = row_ok & (available >= need)
    short = np.flatnonzero(~enough)
    if len(short):
        reasons[short] = frame.history_reasons(short)
    rows = np.flatnonzero(enough)
    if len(rows):
        refits, which = np.unique(refit_row[rows], return_inverse=True)
        tables = [_fit_table(frame, int(r), p, z, lag) for r in refits]
        cell = next_cell[rows]

        def gather(key):
            return np.stack([t[key] for t in tables])[which, cell]

        cols["cce_refit_bar_open_ms"][rows] = np.array([t["refit_bar_open_ms"] for t in tables],
                                                       dtype="float64")[which]
        cols["cce_training_first_open_ms"][rows] = np.array([t["training_first_open_ms"] for t in tables],
                                                            dtype="float64")[which]
        for col, key in (("cce_gross_mean", "mean"), ("cce_long_net_edge", "long_net"),
                         ("cce_short_net_edge", "short_net"), ("cce_long_lower_bound", "long_lb"),
                         ("cce_short_lower_bound", "short_lb"), ("cce_raw_obs", "raw"), ("cce_weekly_obs", "n"),
                         ("cce_effective_obs", "n_eff"), ("cce_std_error", "se"),
                         ("cce_cell_decision", "decision")):
            cols[col][rows] = gather(key).astype("float64")
        decision = gather("decision").astype(np.int64)
        valid[rows] = True
        signal[rows] = decision
        reasons[rows] = np.array(CELL_REASONS, dtype=object)[gather("reason")]

    cutoff, declared_boundary, runtime = _decision_time(result.attrs)
    if runtime:
        i = n - 1
        if cutoff is not None:
            cols["cce_decision_cutoff_ms"][i] = float(cutoff)
        if declared_boundary is not None:
            cols["cce_decision_boundary_ms"][i] = float(declared_boundary)
        if valid[i]:
            row_boundary = int(frame.open_ms[i]) + interval
            held = ""
            if cutoff is None or declared_boundary is None:
                held = "decision_time_missing"
            elif row_boundary != declared_boundary:
                held = "decision_boundary_mismatch"
            elif cutoff - row_boundary < 0:
                held = "decision_bar_future"
            elif cutoff - row_boundary >= interval:
                held = "decision_bar_stale"
            if held:
                valid[i] = False
                signal[i] = 0
                reasons[i] = held

    for col in VALUE_COLUMNS:
        result[col] = cols[col]
    result["cce_interval_ms"] = np.full(n, float(interval))
    result["cce_valid"] = valid.astype("float64")
    result["cce_reason"] = reasons
    result["cce_reason_code"] = np.array([float(REASON_CODES[r]) for r in reasons], dtype="float64")
    result["signal"] = signal
    return result


def _cell_rows(table: dict) -> list:
    rows = []
    for cell in range(CELLS):
        def num(key):
            v = float(table[key][cell])
            return v if math.isfinite(v) else None
        rows.append({
            "cell": cell, "weekday": cell // 24, "hour": cell % 24, "weekly_obs": int(table["n"][cell]),
            "raw_obs": int(table["raw"][cell]), "gross_mean": num("mean"), "std_error": num("se"),
            "effective_obs": num("n_eff"), "long_net_edge": num("long_net"), "short_net_edge": num("short_net"),
            "long_lower_bound": num("long_lb"), "short_lower_bound": num("short_lb"),
            "decision": int(table["decision"][cell]), "reason": CELL_REASONS[int(table["reason"][cell])],
        })
    return rows


def _finite_max(values):
    finite = values[np.isfinite(values)]
    return float(finite.max()) if len(finite) else None


def calendar_refit_tables(df: pd.DataFrame, cells: bool = True, **params) -> list:
    p = validate_calendar_params(params)
    frame, reason = _prepare(df, p)
    if reason:
        return []
    alpha, z = corrected_threshold(p["confidence"])
    lag = newey_west_lag(p["holding_bars"], p["expected_interval_ms"])
    T = p["training_bars"]
    out = []
    for r in range(frame.n):
        if frame.row_bad[r]:
            continue
        boundary = int(frame.open_ms[r]) + frame.interval
        grid = p["refit_bars"] * frame.interval
        if (boundary - EPOCH_MS) % grid != 0:
            continue
        if r - int(frame.run_start[r]) + 1 < T:
            continue
        table = _fit_table(frame, r, p, z, lag)
        item = {
            "refit_boundary_ms": table["refit_boundary_ms"], "refit_bar_open_ms": table["refit_bar_open_ms"],
            "training_first_open_ms": table["training_first_open_ms"], "labels": table["labels"],
            "last_label_exit_open_ms": table["last_label_exit_open_ms"], "weeks_spanned": table["nweeks"],
            "family_size": FAMILY_SIZE, "corrected_alpha": alpha, "corrected_z": z, "nw_lag": lag,
            "eligible_long": int((table["decision"] == 1).sum()), "eligible_short": int((table["decision"] == -1).sum()),
            "max_long_lower_bound": _finite_max(table["long_lb"]), "max_short_lower_bound": _finite_max(table["short_lb"]),
            "reasons": {name: int((table["reason"] == k).sum()) for k, name in enumerate(CELL_REASONS)
                        if int((table["reason"] == k).sum())},
        }
        if cells:
            item["cells"] = _cell_rows(table)
        out.append(item)
    return out

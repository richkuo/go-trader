"""
Consolidation characterization study (offline research).

Scans historical OHLCV for one symbol+timeframe, segments consolidation
("range") periods three different ways, benchmarks the detectors against each
other, measures each episode (duration, box geometry, shape metrics), sizes the
breakout candle under three definitions, correlates shape against breakout
behavior, and writes a per-episode table, an aggregate summary, and charts.

Research only — no live-strategy / scheduler / registry wiring.

Usage:
    uv run --no-sync python backtest/consolidation_research.py \
        --symbol BTC/USDT --timeframe 1h --since 2023-01-01 \
        --exchange-id binanceus --out-dir <dir>
"""

import argparse
import csv
import datetime
import json
import os
import sys
from dataclasses import dataclass, asdict, field
from typing import Callable, Dict, List, Optional, Tuple

import numpy as np
import pandas as pd

# shared_tools carries the data fetcher (same wiring as run_backtest.py).
sys.path.insert(0, os.path.join(os.path.dirname(__file__), "..", "shared_tools"))


# --------------------------------------------------------------------------- #
# Core data structures
# --------------------------------------------------------------------------- #


@dataclass
class Episode:
    """A detected consolidation period as a half-open bar range [start, end)."""

    start_idx: int
    end_idx: int  # exclusive
    method: str = ""

    @property
    def n_bars(self) -> int:
        return self.end_idx - self.start_idx


# --------------------------------------------------------------------------- #
# Indicators
# --------------------------------------------------------------------------- #


def true_range(df: pd.DataFrame) -> pd.Series:
    high, low, close = df["high"], df["low"], df["close"]
    prev_close = close.shift(1)
    tr = pd.concat(
        [high - low, (high - prev_close).abs(), (low - prev_close).abs()],
        axis=1,
    ).max(axis=1)
    return tr


def atr(df: pd.DataFrame, period: int = 14) -> pd.Series:
    # Mirror production standard_atr (#887): whole-number round only for
    # BTC-scale assets (ATR >= 100) so research geometry matches the live
    # stamped ATR; sub-100 assets pass through unrounded.
    series = true_range(df).rolling(window=period, min_periods=1).mean()
    return series.where(series < 100, series.round(0))


# --------------------------------------------------------------------------- #
# Detectors:  df + params -> list[Episode]   (pure)
# --------------------------------------------------------------------------- #


def _coalesce_mask(mask: np.ndarray, min_bars: int, method: str) -> List[Episode]:
    """Turn a boolean per-bar 'in consolidation' mask into episodes."""
    episodes: List[Episode] = []
    n = len(mask)
    i = 0
    while i < n:
        if mask[i]:
            j = i
            while j < n and mask[j]:
                j += 1
            if j - i >= min_bars:
                episodes.append(Episode(start_idx=i, end_idx=j, method=method))
            i = j
        else:
            i += 1
    return episodes


def detect_range_containment(
    df: pd.DataFrame,
    min_bars: int = 8,
    box_width_pct: float = 0.04,
    atr_period: int = 14,
    **_: object,
) -> List[Episode]:
    """A bar is 'in range' when the rolling high-low span over the trailing
    ``min_bars`` window stays within ``box_width_pct`` of the window's mid price.
    """
    high, low, close = df["high"], df["low"], df["close"]
    roll_hi = high.rolling(window=min_bars, min_periods=min_bars).max()
    roll_lo = low.rolling(window=min_bars, min_periods=min_bars).min()
    mid = (roll_hi + roll_lo) / 2.0
    width = (roll_hi - roll_lo) / mid.replace(0, np.nan)
    mask = (width <= box_width_pct).fillna(False).to_numpy()
    return _coalesce_mask(mask, min_bars, "range_containment")


def detect_volatility_contraction(
    df: pd.DataFrame,
    min_bars: int = 8,
    bandwidth_threshold: float = 0.7,
    bb_period: int = 20,
    bb_std: float = 2.0,
    **_: object,
) -> List[Episode]:
    """A bar is 'in range' when Bollinger bandwidth is below ``bandwidth_threshold``
    times its own trailing-median bandwidth (a relative squeeze).
    """
    close = df["close"]
    mid = close.rolling(window=bb_period, min_periods=bb_period).mean()
    std = close.rolling(window=bb_period, min_periods=bb_period).std()
    bandwidth = (2 * bb_std * std) / mid.replace(0, np.nan)
    baseline = bandwidth.rolling(window=bb_period * 3, min_periods=bb_period).median()
    mask = (bandwidth <= bandwidth_threshold * baseline).fillna(False).to_numpy()
    return _coalesce_mask(mask, min_bars, "volatility_contraction")


def detect_regression_flatness(
    df: pd.DataFrame,
    min_bars: int = 8,
    flatness_slope: float = 0.0006,
    flatness_residual: float = 0.02,
    **_: object,
) -> List[Episode]:
    """A bar is 'in range' when a rolling linear fit of close over the trailing
    ``min_bars`` window has |slope| (normalized per-bar return) below
    ``flatness_slope`` AND normalized residual scatter below ``flatness_residual``.
    """
    close = df["close"].to_numpy(dtype=float)
    n = len(close)
    x = np.arange(min_bars, dtype=float)
    x_mean = x.mean()
    x_centered = x - x_mean
    x_var = (x_centered ** 2).sum()
    mask = np.zeros(n, dtype=bool)
    for end in range(min_bars, n + 1):
        y = close[end - min_bars : end]
        y_mean = y.mean()
        if y_mean == 0:
            continue
        slope = (x_centered * (y - y_mean)).sum() / x_var
        fit = y_mean + slope * x_centered
        residual = np.sqrt(((y - fit) ** 2).mean()) / y_mean
        slope_norm = abs(slope) / y_mean
        if slope_norm <= flatness_slope and residual <= flatness_residual:
            mask[end - 1] = True
    return _coalesce_mask(mask, min_bars, "regression_flatness")


DETECTORS: Dict[str, Callable[..., List[Episode]]] = {
    "range_containment": detect_range_containment,
    "volatility_contraction": detect_volatility_contraction,
    "regression_flatness": detect_regression_flatness,
}

# Params each detector's *detection* actually depends on. Used to cache episode
# lists across sweep cells: when only the box params change, the (slow)
# regression-flatness and volatility detectors are reused instead of recomputed.
# (escape_k / atr_period affect scoring, not detection, so they are excluded.)
DETECTOR_PARAM_KEYS: Dict[str, Tuple[str, ...]] = {
    "range_containment": ("min_bars", "box_width_pct"),
    "volatility_contraction": ("min_bars", "bandwidth_threshold"),
    "regression_flatness": ("min_bars", "flatness_slope", "flatness_residual"),
}


def _detector_cache_key(name: str, params: dict):
    return (name,) + tuple(params.get(k) for k in DETECTOR_PARAM_KEYS.get(name, ()))


# --------------------------------------------------------------------------- #
# Per-episode measurement
# --------------------------------------------------------------------------- #


def measure_box(df: pd.DataFrame, ep: Episode) -> Dict[str, float]:
    seg = df.iloc[ep.start_idx : ep.end_idx]
    top = float(seg["high"].max())
    bottom = float(seg["low"].min())
    mid = (top + bottom) / 2.0
    return {
        "top": top,
        "bottom": bottom,
        "mid": mid,
        "mean_close": float(seg["close"].mean()),
        "range_max": top,
        "range_min": bottom,
        "range_avg": float((seg["high"] + seg["low"]).mean() / 2.0),
        "width_pct": (top - bottom) / mid if mid else float("nan"),
    }


def _linfit_slope(y: np.ndarray) -> float:
    n = len(y)
    if n < 2:
        return 0.0
    x = np.arange(n, dtype=float)
    xc = x - x.mean()
    denom = (xc ** 2).sum()
    if denom == 0:
        return 0.0
    return float((xc * (y - y.mean())).sum() / denom)


def measure_shape(df: pd.DataFrame, ep: Episode) -> Dict[str, float]:
    seg = df.iloc[ep.start_idx : ep.end_idx]
    highs = seg["high"].to_numpy(dtype=float)
    lows = seg["low"].to_numpy(dtype=float)
    closes = seg["close"].to_numpy(dtype=float)
    mid = (highs.max() + lows.min()) / 2.0
    norm = mid if mid else 1.0

    top_slope = _linfit_slope(highs) / norm
    bottom_slope = _linfit_slope(lows) / norm

    # Edge travel: how far each edge moved across the whole episode, as a fraction
    # of box height. Scale-free; used to classify named patterns.
    box_height = float(highs.max() - lows.min())
    n = len(seg)
    top_travel = (_linfit_slope(highs) * n / box_height) if box_height else 0.0
    bottom_travel = (_linfit_slope(lows) * n / box_height) if box_height else 0.0

    # width contraction: mean span of first third vs last third.
    third = max(1, len(seg) // 3)
    start_w = float((highs[:third] - lows[:third]).mean())
    end_w = float((highs[-third:] - lows[-third:]).mean())
    contraction = end_w / start_w if start_w else float("nan")

    # time-in-zone skew: fraction of bars whose close sits in top/mid/bottom third.
    top, bottom = highs.max(), lows.min()
    span = top - bottom
    if span > 0:
        pos = (closes - bottom) / span
        bottom_frac = float((pos < 1 / 3).mean())
        mid_frac = float(((pos >= 1 / 3) & (pos < 2 / 3)).mean())
        top_frac = float((pos >= 2 / 3).mean())
    else:
        bottom_frac = mid_frac = top_frac = float("nan")

    return {
        "top_edge_slope": top_slope,
        "bottom_edge_slope": bottom_slope,
        "top_edge_travel": top_travel,
        "bottom_edge_travel": bottom_travel,
        "width_contraction": contraction,
        "time_top_frac": top_frac,
        "time_mid_frac": mid_frac,
        "time_bottom_frac": bottom_frac,
    }


# Edge moved less than this fraction of box height across the episode => "flat".
_PATTERN_FLAT_THRESHOLD = 0.25


def classify_pattern(shape: Dict[str, float]) -> str:
    """Classify an episode into a named chart pattern from its edge travel.

    Uses the standard consolidation taxonomy: rectangle, ascending/descending/
    symmetrical triangle, rising/falling wedge, broadening. `flat` = an edge that
    moved < _PATTERN_FLAT_THRESHOLD of the box height across the episode.
    """
    t = shape.get("top_edge_travel", 0.0)
    b = shape.get("bottom_edge_travel", 0.0)
    flat = _PATTERN_FLAT_THRESHOLD

    def sign(x):
        return "flat" if abs(x) < flat else ("up" if x > 0 else "down")

    ts, bs = sign(t), sign(b)
    if ts == "flat" and bs == "flat":
        return "rectangle"
    if ts == "flat" and bs == "up":
        return "ascending_triangle"
    if ts == "down" and bs == "flat":
        return "descending_triangle"
    if ts == "down" and bs == "up":
        return "symmetrical_triangle"
    if ts == "up" and bs == "down":
        return "broadening"
    if ts == "up" and bs == "up":
        return "rising_wedge"  # incl. up-channel
    if ts == "down" and bs == "down":
        return "falling_wedge"  # incl. down-channel
    # one edge flat, other diverging.
    return "rectangle_drift"


def measure_volume_profile(
    df: pd.DataFrame, ep: Episode, bins: int = 24, value_area_frac: float = 0.70
) -> Dict[str, float]:
    """Volume-profile stats for an episode (Market Profile / Wyckoff lens).

    Bins the box price range, accumulates each bar's volume at its typical price,
    and reports the Point of Control (POC, highest-volume price), the Value Area
    high/low (VAH/VAL, the band holding ``value_area_frac`` of volume around POC),
    and where the POC sits within the box (0=bottom, 1=top).
    """
    seg = df.iloc[ep.start_idx : ep.end_idx]
    out = {
        "poc": float("nan"), "vah": float("nan"), "val": float("nan"),
        "poc_position": float("nan"), "value_area_width_frac": float("nan"),
    }
    top = float(seg["high"].max())
    bottom = float(seg["low"].min())
    if top <= bottom or "volume" not in seg:
        return out

    typical = (seg["high"] + seg["low"] + seg["close"]) / 3.0
    edges = np.linspace(bottom, top, bins + 1)
    idx = np.clip(np.digitize(typical.to_numpy(), edges) - 1, 0, bins - 1)
    vol = seg["volume"].to_numpy(dtype=float)
    hist = np.zeros(bins)
    for i, v in zip(idx, vol):
        hist[i] += v
    centers = (edges[:-1] + edges[1:]) / 2.0

    total = hist.sum()
    if total <= 0:
        return out
    poc_bin = int(hist.argmax())
    out["poc"] = float(centers[poc_bin])
    out["poc_position"] = (out["poc"] - bottom) / (top - bottom)

    # Expand outward from the POC bin until value_area_frac of volume is covered.
    lo = hi = poc_bin
    covered = hist[poc_bin]
    target = value_area_frac * total
    while covered < target and (lo > 0 or hi < bins - 1):
        left = hist[lo - 1] if lo > 0 else -1.0
        right = hist[hi + 1] if hi < bins - 1 else -1.0
        if right >= left:
            hi += 1
            covered += hist[hi]
        else:
            lo -= 1
            covered += hist[lo]
    out["val"] = float(edges[lo])
    out["vah"] = float(edges[hi + 1])
    out["value_area_width_frac"] = (out["vah"] - out["val"]) / (top - bottom)
    return out


def measure_escape_candle(
    df: pd.DataFrame,
    ep: Episode,
    atr_series: pd.Series,
    escape_k: float = 1.5,
    edge_margin_pct: float = 0.002,
) -> Dict[str, float]:
    """Characterize the first candle after the episode under three definitions.

    Returns the escape candle's true range in price + which definitions it
    satisfies, plus the breakout direction (sign of the escape candle).
    """
    seg = df.iloc[ep.start_idx : ep.end_idx]
    n = len(df)
    out: Dict[str, float] = {
        "escape_idx": float("nan"),
        "escape_tr": float("nan"),
        "escape_k_vs_median_tr": float("nan"),
        "escape_k_vs_atr": float("nan"),
        "escape_by_median_tr": 0.0,
        "escape_by_atr": 0.0,
        "escape_by_edge": 0.0,
        "breakout_direction": 0.0,
    }
    if ep.end_idx >= n:
        return out

    esc = df.iloc[ep.end_idx]
    esc_tr = float(true_range(df).iloc[ep.end_idx])
    median_in_range_tr = float(true_range(seg).median())
    atr_at = float(atr_series.iloc[ep.end_idx])

    top = float(seg["high"].max())
    bottom = float(seg["low"].min())

    out["escape_idx"] = float(ep.end_idx)
    out["escape_tr"] = esc_tr
    if median_in_range_tr > 0:
        out["escape_k_vs_median_tr"] = esc_tr / median_in_range_tr
        out["escape_by_median_tr"] = float(esc_tr >= escape_k * median_in_range_tr)
    if atr_at > 0:
        out["escape_k_vs_atr"] = esc_tr / atr_at
        out["escape_by_atr"] = float(esc_tr >= escape_k * atr_at)

    close = float(esc["close"])
    if close > top * (1 + edge_margin_pct):
        out["escape_by_edge"] = 1.0
        out["breakout_direction"] = 1.0
    elif close < bottom * (1 - edge_margin_pct):
        out["escape_by_edge"] = 1.0
        out["breakout_direction"] = -1.0
    else:
        # direction from where price went next bar relative to the box mid.
        out["breakout_direction"] = 1.0 if close >= (top + bottom) / 2 else -1.0
    return out


# --------------------------------------------------------------------------- #
# Detector benchmark
# --------------------------------------------------------------------------- #


def benchmark_detectors(
    df: pd.DataFrame, params: dict, detector_cache: Optional[dict] = None
) -> Tuple[Dict[str, List[Episode]], pd.DataFrame]:
    """Run all detectors, score each on coverage / tightness / false-break rate.

    ``detector_cache`` (optional, caller-owned) memoizes episode lists across
    calls keyed by each detector's own detection params, so a sweep that varies
    only one detector's params doesn't recompute the others every cell.
    """
    atr_series = atr(df, params.get("atr_period", 14))
    median_tr = float(true_range(df).median()) or 1.0
    results: Dict[str, List[Episode]] = {}
    rows = []
    for name, fn in DETECTORS.items():
        if detector_cache is not None:
            ckey = _detector_cache_key(name, params)
            if ckey in detector_cache:
                eps = detector_cache[ckey]
            else:
                eps = fn(df, **params)
                detector_cache[ckey] = eps
        else:
            eps = fn(df, **params)
        results[name] = eps
        if not eps:
            rows.append(
                {"method": name, "n_episodes": 0, "avg_bars": 0.0,
                 "avg_width_pct": float("nan"), "false_break_rate": float("nan"),
                 "coverage_pct": 0.0}
            )
            continue
        widths, durations, false_breaks = [], [], 0
        for ep in eps:
            box = measure_box(df, ep)
            widths.append(box["width_pct"])
            durations.append(ep.n_bars)
            esc = measure_escape_candle(df, ep, atr_series, params.get("escape_k", 1.5))
            # false break: flagged escape candle barely larger than in-range noise.
            if esc["escape_by_edge"] == 0.0 and esc["escape_by_atr"] == 0.0:
                false_breaks += 1
        rows.append(
            {
                "method": name,
                "n_episodes": len(eps),
                "avg_bars": float(np.mean(durations)),
                "avg_width_pct": float(np.nanmean(widths)),
                "false_break_rate": false_breaks / len(eps),
                "coverage_pct": sum(durations) / len(df),
            }
        )
    bench = pd.DataFrame(rows)
    # Primary = most episodes with a real escape (lowest false-break), tie-break tightness.
    if not bench.empty:
        bench["score"] = (
            (1 - bench["false_break_rate"].fillna(1.0))
            * bench["n_episodes"]
            / (1 + bench["avg_width_pct"].fillna(1.0))
        )
    return results, bench


# --------------------------------------------------------------------------- #
# Shape vs breakout correlation
# --------------------------------------------------------------------------- #


def correlate_shape_breakout(episodes_df: pd.DataFrame) -> Dict[str, object]:
    if episodes_df.empty:
        return {}
    metrics = [
        "top_edge_slope",
        "bottom_edge_slope",
        "width_contraction",
        "width_pct",
        "n_bars",
        "poc_position",
        "value_area_width_frac",
    ]
    targets = {
        "breakout_direction": episodes_df["breakout_direction"],
        "escape_k_vs_atr": episodes_df["escape_k_vs_atr"],
    }
    out: Dict[str, object] = {"pearson": {}, "spearman": {}, "grouped": {}}
    for tname, tvals in targets.items():
        out["pearson"][tname] = {}
        out["spearman"][tname] = {}
        for m in metrics:
            if m not in episodes_df:
                continue
            valid = episodes_df[[m]].assign(t=tvals).dropna()
            if len(valid) < 3:
                continue
            out["pearson"][tname][m] = float(valid[m].corr(valid["t"]))
            # Spearman = Pearson on ranks (avoids a scipy dependency).
            out["spearman"][tname][m] = float(
                valid[m].rank().corr(valid["t"].rank())
            )
    # grouped: median escape size by contraction tercile.
    wc = episodes_df.dropna(subset=["width_contraction", "escape_k_vs_atr"])
    if len(wc) >= 6:
        try:
            wc = wc.assign(bucket=pd.qcut(wc["width_contraction"], 3,
                                          labels=["tight", "mid", "wide"]))
            grp = wc.groupby("bucket", observed=True)["escape_k_vs_atr"].median()
            out["grouped"]["escape_by_contraction_tercile"] = {
                str(k): float(v) for k, v in grp.items()
            }
        except (ValueError, IndexError):
            pass

    # Named-pattern breakdown: count, breakout up-rate, median escape size.
    if "pattern" in episodes_df:
        pats = {}
        for name, g in episodes_df.groupby("pattern"):
            up = (g["breakout_direction"] > 0).mean()
            pats[str(name)] = {
                "count": int(len(g)),
                "breakout_up_rate": float(up),
                "escape_xatr_median": float(g["escape_k_vs_atr"].median()),
            }
        out["patterns"] = pats
    return out


# --------------------------------------------------------------------------- #
# Assemble per-episode table
# --------------------------------------------------------------------------- #


def build_episode_table(
    df: pd.DataFrame, episodes: List[Episode], params: dict
) -> pd.DataFrame:
    atr_series = atr(df, params.get("atr_period", 14))
    escape_k = params.get("escape_k", 1.5)
    rows = []
    for ep in episodes:
        seg = df.iloc[ep.start_idx : ep.end_idx]
        row: Dict[str, object] = {
            "method": ep.method,
            "start": seg.index[0],
            "end": seg.index[-1],
            "n_bars": ep.n_bars,
        }
        if isinstance(seg.index, pd.DatetimeIndex):
            row["duration"] = str(seg.index[-1] - seg.index[0])
        row.update(measure_box(df, ep))
        shape = measure_shape(df, ep)
        row.update(shape)
        row["pattern"] = classify_pattern(shape)
        row.update(measure_volume_profile(df, ep))
        row.update(measure_escape_candle(df, ep, atr_series, escape_k))
        rows.append(row)
    return pd.DataFrame(rows)


# --------------------------------------------------------------------------- #
# Reporting
# --------------------------------------------------------------------------- #


def render_report(
    df: pd.DataFrame,
    bench: pd.DataFrame,
    primary_method: str,
    episodes: List[Episode],
    episodes_df: pd.DataFrame,
    correlations: Dict[str, object],
    out_dir: str,
    symbol: str,
    timeframe: str,
) -> Dict[str, str]:
    os.makedirs(out_dir, exist_ok=True)
    paths: Dict[str, str] = {}

    # Per-episode table.
    csv_path = os.path.join(out_dir, "episodes.csv")
    episodes_df.to_csv(csv_path, index=False)
    paths["episodes_csv"] = csv_path

    json_path = os.path.join(out_dir, "episodes.json")
    episodes_df.to_json(json_path, orient="records", date_format="iso", indent=2)
    paths["episodes_json"] = json_path

    # Aggregate summary.
    summary = {
        "symbol": symbol,
        "timeframe": timeframe,
        "n_bars_analyzed": int(len(df)),
        "primary_method": primary_method,
        "benchmark": bench.to_dict(orient="records"),
        "correlations": correlations,
    }
    if not episodes_df.empty:
        summary["distributions"] = {
            "duration_bars": {
                "median": float(episodes_df["n_bars"].median()),
                "p25": float(episodes_df["n_bars"].quantile(0.25)),
                "p75": float(episodes_df["n_bars"].quantile(0.75)),
            },
            "width_pct": {
                "median": float(episodes_df["width_pct"].median()),
                "p25": float(episodes_df["width_pct"].quantile(0.25)),
                "p75": float(episodes_df["width_pct"].quantile(0.75)),
            },
            "escape_k_vs_atr": {
                "median": float(episodes_df["escape_k_vs_atr"].median()),
                "p25": float(episodes_df["escape_k_vs_atr"].quantile(0.25)),
                "p75": float(episodes_df["escape_k_vs_atr"].quantile(0.75)),
            },
        }
    summary_path = os.path.join(out_dir, "summary.json")
    with open(summary_path, "w") as f:
        json.dump(summary, f, indent=2, default=str)
    paths["summary_json"] = summary_path

    # Charts.
    paths.update(
        _render_charts(df, episodes, episodes_df, out_dir, symbol, timeframe)
    )
    return paths


def _render_charts(df, episodes, episodes_df, out_dir, symbol, timeframe):
    import matplotlib

    matplotlib.use("Agg")
    import matplotlib.pyplot as plt
    from matplotlib.patches import Rectangle

    paths = {}

    # Price with boxes + breakout markers.
    fig, ax = plt.subplots(figsize=(16, 7))
    ax.plot(df.index, df["close"], color="#222", lw=0.6, label="close")
    for ep in episodes:
        seg = df.iloc[ep.start_idx : ep.end_idx]
        top, bottom = seg["high"].max(), seg["low"].min()
        ax.add_patch(
            Rectangle(
                (seg.index[0], bottom),
                seg.index[-1] - seg.index[0],
                top - bottom,
                facecolor="#4c9aff",
                alpha=0.25,
                edgecolor="#1f6feb",
                lw=0.5,
            )
        )
        if ep.end_idx < len(df):
            esc_t = df.index[ep.end_idx]
            ax.scatter([esc_t], [df["close"].iloc[ep.end_idx]],
                       color="red", s=14, zorder=5)
    ax.set_title(f"{symbol} {timeframe} — consolidations + escape candles")
    ax.legend(loc="upper left")
    p = os.path.join(out_dir, "price_boxes.png")
    fig.savefig(p, dpi=120, bbox_inches="tight")
    plt.close(fig)
    paths["chart_price_boxes"] = p

    if episodes_df.empty:
        return paths

    # Distribution histograms.
    fig, axes = plt.subplots(1, 3, figsize=(16, 4))
    for ax, col, title in zip(
        axes,
        ["n_bars", "width_pct", "escape_k_vs_atr"],
        ["duration (bars)", "width (%)", "escape size (x ATR)"],
    ):
        vals = episodes_df[col].dropna()
        if len(vals):
            ax.hist(vals, bins=min(20, max(5, len(vals))), color="#4c9aff",
                    edgecolor="#1f6feb")
        ax.set_title(title)
    fig.suptitle(f"{symbol} {timeframe} — episode distributions")
    p = os.path.join(out_dir, "distributions.png")
    fig.savefig(p, dpi=120, bbox_inches="tight")
    plt.close(fig)
    paths["chart_distributions"] = p

    # Shape vs breakout scatter.
    fig, ax = plt.subplots(figsize=(8, 6))
    sub = episodes_df.dropna(subset=["width_contraction", "escape_k_vs_atr"])
    if len(sub):
        colors = ["#2da44e" if d > 0 else "#cf222e"
                  for d in sub["breakout_direction"]]
        ax.scatter(sub["width_contraction"], sub["escape_k_vs_atr"],
                   c=colors, alpha=0.7)
        ax.set_xlabel("width contraction (end/start)")
        ax.set_ylabel("escape size (x ATR)")
        ax.set_title(f"{symbol} {timeframe} — shape vs breakout "
                     "(green=up, red=down)")
    p = os.path.join(out_dir, "shape_vs_breakout.png")
    fig.savefig(p, dpi=120, bbox_inches="tight")
    plt.close(fig)
    paths["chart_shape_vs_breakout"] = p
    return paths


# --------------------------------------------------------------------------- #
# Runs-CSV tracking (auto-append one row per detector method)
# --------------------------------------------------------------------------- #

RUNS_CSV_COLUMNS = [
    "run_id", "date", "symbol", "timeframe", "since", "bars", "method",
    "is_primary", "n_episodes", "avg_bars", "avg_width_pct", "false_break_rate",
    "coverage_pct", "score", "dur_median_bars", "width_pct_median",
    "escape_xatr_median", "corr_contraction_vs_escape", "params", "out_dir",
    "takeaway",
]

# Which detector params to record in the per-method `params` cell.
_METHOD_PARAM_KEYS = {
    "range_containment": ["min_bars", "box_width_pct", "escape_k", "atr_period"],
    "volatility_contraction": ["bandwidth_threshold", "escape_k"],
    "regression_flatness": ["flatness_slope", "flatness_residual", "escape_k"],
}


def next_run_id(runs_csv: str) -> str:
    """Return a zero-padded run id one greater than the max numeric id on file."""
    max_id = 0
    if os.path.exists(runs_csv):
        with open(runs_csv, newline="") as f:
            for row in csv.DictReader(f):
                try:
                    max_id = max(max_id, int(row.get("run_id", 0)))
                except (TypeError, ValueError):
                    continue
    return f"{max_id + 1:03d}"


def _fmt(v: object, nd: int = 4) -> object:
    return round(v, nd) if isinstance(v, float) and not np.isnan(v) else (
        "" if isinstance(v, float) and np.isnan(v) else v
    )


def append_runs_csv(
    runs_csv: str,
    run_id: str,
    run_date: str,
    args: argparse.Namespace,
    bars: int,
    res: Dict[str, object],
    only_methods: Optional[List[str]] = None,
) -> None:
    """Append one row per detector method to the runs CSV (header on first write).

    ``only_methods`` restricts which detector rows are written (e.g. a sweep that
    only cares about ``range_containment``).
    """
    bench = res["benchmark"]
    primary = res["primary_method"]
    edf = res["episodes_df"]
    corr = res["correlations"].get("pearson", {}).get("escape_k_vs_atr", {})

    dur_med = width_med = esc_med = ""
    if not edf.empty:
        dur_med = _fmt(float(edf["n_bars"].median()), 1)
        width_med = _fmt(float(edf["width_pct"].median()))
        esc_med = _fmt(float(edf["escape_k_vs_atr"].median()), 3)

    rows = []
    bench_by_method = {r["method"]: r for r in bench.to_dict(orient="records")}
    methods = only_methods if only_methods is not None else list(DETECTORS)
    for method in methods:
        b = bench_by_method.get(method, {})
        is_primary = method == primary
        params = ";".join(
            f"{k}={getattr(args, k)}"
            for k in _METHOD_PARAM_KEYS.get(method, [])
            if hasattr(args, k)
        )
        rows.append({
            "run_id": run_id,
            "date": run_date,
            "symbol": args.symbol,
            "timeframe": args.timeframe,
            "since": args.since,
            "bars": bars,
            "method": method,
            "is_primary": int(is_primary),
            "n_episodes": b.get("n_episodes", ""),
            "avg_bars": _fmt(b.get("avg_bars", "")),
            "avg_width_pct": _fmt(b.get("avg_width_pct", "")),
            "false_break_rate": _fmt(b.get("false_break_rate", "")),
            "coverage_pct": _fmt(b.get("coverage_pct", "")),
            "score": _fmt(b.get("score", "")),
            "dur_median_bars": dur_med if is_primary else "",
            "width_pct_median": width_med if is_primary else "",
            "escape_xatr_median": esc_med if is_primary else "",
            "corr_contraction_vs_escape": (
                _fmt(corr.get("width_contraction", "")) if is_primary else ""
            ),
            "params": params,
            "out_dir": args.out_dir,
            "takeaway": "",  # filled in by hand after reviewing the run
        })

    os.makedirs(os.path.dirname(runs_csv) or ".", exist_ok=True)
    write_header = not os.path.exists(runs_csv) or os.path.getsize(runs_csv) == 0
    with open(runs_csv, "a", newline="") as f:
        w = csv.DictWriter(f, fieldnames=RUNS_CSV_COLUMNS)
        if write_header:
            w.writeheader()
        w.writerows(rows)


# --------------------------------------------------------------------------- #
# CLI
# --------------------------------------------------------------------------- #


def run(
    df: pd.DataFrame,
    params: dict,
    out_dir: str,
    symbol: str,
    timeframe: str,
    primary_method: Optional[str] = None,
    write_report: bool = True,
    detector_cache: Optional[dict] = None,
) -> Dict[str, object]:
    results, bench = benchmark_detectors(df, params, detector_cache=detector_cache)
    if primary_method is None:
        primary_method = (
            str(bench.sort_values("score", ascending=False).iloc[0]["method"])
            if not bench.empty and "score" in bench and bench["score"].notna().any()
            else next(iter(DETECTORS))
        )
    episodes = results[primary_method]
    episodes_df = build_episode_table(df, episodes, params)
    correlations = correlate_shape_breakout(episodes_df)
    paths = (
        render_report(
            df, bench, primary_method, episodes, episodes_df,
            correlations, out_dir, symbol, timeframe,
        )
        if write_report
        else {}
    )
    return {
        "primary_method": primary_method,
        "n_episodes": len(episodes),
        "benchmark": bench,
        "paths": paths,
        "correlations": correlations,
        "episodes_df": episodes_df,
    }


def main(argv: Optional[List[str]] = None) -> int:
    p = argparse.ArgumentParser(description="Consolidation characterization study")
    p.add_argument("--symbol", default="BTC/USDT")
    p.add_argument("--timeframe", default="1h")
    p.add_argument("--since", default="2023-01-01")
    p.add_argument("--exchange-id", default="binanceus")
    p.add_argument(
        "--out-dir", default=None,
        help="output dir; defaults to a per-run dir under backtest/consolidation_out/",
    )
    p.add_argument("--min-bars", type=int, default=8)
    p.add_argument("--box-width-pct", type=float, default=0.04)
    p.add_argument("--bandwidth-threshold", type=float, default=0.7)
    p.add_argument("--flatness-slope", type=float, default=0.0006)
    p.add_argument("--flatness-residual", type=float, default=0.02)
    p.add_argument("--escape-k", type=float, default=1.5)
    p.add_argument("--atr-period", type=int, default=14)
    p.add_argument(
        "--runs-csv", default="docs/research/consolidation_runs.csv",
        help="append a per-method row to this CSV after the run; '' disables",
    )
    p.add_argument("--run-id", default=None,
                   help="run id for the CSV; auto-increments if omitted")
    args = p.parse_args(argv)

    run_id = args.run_id or (
        next_run_id(args.runs_csv) if args.runs_csv else "000"
    )
    if args.out_dir is None:
        sym = args.symbol.replace("/", "").lower()
        args.out_dir = os.path.join(
            "backtest", "consolidation_out",
            f"run{run_id}_{sym}_{args.timeframe}",
        )

    from data_fetcher import fetch_full_history

    df = fetch_full_history(
        symbol=args.symbol,
        timeframe=args.timeframe,
        since=args.since,
        exchange_id=args.exchange_id,
    )
    if df.empty:
        print(json.dumps({"error": "no data"}))
        return 1

    params = {
        "min_bars": args.min_bars,
        "box_width_pct": args.box_width_pct,
        "bandwidth_threshold": args.bandwidth_threshold,
        "flatness_slope": args.flatness_slope,
        "flatness_residual": args.flatness_residual,
        "escape_k": args.escape_k,
        "atr_period": args.atr_period,
    }
    res = run(df, params, args.out_dir, args.symbol, args.timeframe)

    print(f"\n=== {args.symbol} {args.timeframe} — {len(df)} bars ===")
    print("\nDetector benchmark:")
    print(res["benchmark"].to_string(index=False))
    print(f"\nPrimary method: {res['primary_method']}  "
          f"({res['n_episodes']} episodes)")
    edf = res["episodes_df"]
    if not edf.empty:
        print(f"\nDuration (bars): median={edf['n_bars'].median():.0f}  "
              f"width%: median={edf['width_pct'].median():.3f}  "
              f"escape(xATR): median={edf['escape_k_vs_atr'].median():.2f}")
    print("\nCorrelations (Pearson, escape size vs shape):")
    print(json.dumps(res["correlations"].get("pearson", {}).get(
        "escape_k_vs_atr", {}), indent=2))
    print("\nOutputs:")
    for k, v in res["paths"].items():
        print(f"  {k}: {v}")

    if args.runs_csv:
        run_date = datetime.date.today().isoformat()
        append_runs_csv(args.runs_csv, run_id, run_date, args, len(df), res)
        print(f"\nLogged run {run_id} -> {args.runs_csv}")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())

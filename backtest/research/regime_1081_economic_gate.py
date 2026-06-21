"""Economic walk-forward gate for regime-conditioned ATR sizing (#1081).

This is the money-side complement to the regime-model separation gates:
does a regime label stream improve held-out trading outcomes when ATR-sized
SL/TP exits are conditioned on that label, compared with flat ATR sizing?

The harness is read-only. It imports the audit windows/datasets from
``eval_windows.py``, applies one open strategy, injects a causal regime label
stream into ``df["regime"]``, then runs paired flat-vs-regime ATR exit configs
through ``Backtester``. The backtester owns fills, fees, slippage, position
telemetry, and the existing N -> N+1 signal/regime shift.

Usage:

    uv run --no-sync python backtest/research/regime_1081_economic_gate.py
    uv run --no-sync python backtest/research/regime_1081_economic_gate.py \
        --model-json /tmp/regime_1080_btc.json --json /tmp/regime_1081.json
"""
from __future__ import annotations

import argparse
import json
import os
import sys
from collections import OrderedDict
from copy import deepcopy
from typing import Iterable, Optional

import numpy as np

_THIS_DIR = os.path.dirname(os.path.abspath(__file__))
_BACKTEST = os.path.abspath(os.path.join(_THIS_DIR, ".."))
_ROOT = os.path.abspath(os.path.join(_BACKTEST, ".."))
for _p in (_THIS_DIR, _BACKTEST, _ROOT, os.path.join(_ROOT, "shared_tools")):
    if _p not in sys.path:
        sys.path.insert(0, _p)

from atr import ensure_atr_indicator  # noqa: E402
from backtester import Backtester  # noqa: E402
from data_fetcher import load_cached_data  # noqa: E402
from eval_windows import DEFAULT_CAPITAL, PLATFORM, WINDOWS, dd_adjusted_return  # noqa: E402
from registry_loader import load_registry  # noqa: E402
from regime import (  # noqa: E402
    VALID_LABELS_COMPOSITE,
    _DEFAULT_COMPOSITE_THRESHOLDS,
    composite_feature_matrix,
    compute_regime_composite,
)
from regime_hmm import forward_filter_labels  # noqa: E402


DEFAULT_SYMBOL = "BTC/USDT"
DEFAULT_TIMEFRAME = "1h"
DEFAULT_STRATEGY = "squeeze_momentum"
DEFAULT_REGISTRY = "futures"
DEFAULT_DIRECTION = "long"
DEFAULT_WINDOWS = ("is", "oos", "2023", "2024", "2025H1")
DEFAULT_GATE_WINDOWS = ("oos",)
DEFAULT_PERIOD = 48

DEFAULT_TP_TIERS = [
    {"atr_multiple": 1.5, "close_fraction": 0.4},
    {"atr_multiple": 3.0, "close_fraction": 0.8},
    {"atr_multiple": 5.0, "close_fraction": 1.0},
]

SURFACES = (
    "fixed_sl",
    "trailing_stop",
    "tiered_tp",
    "tiered_tp_live",
    "trailing_ratchet",
)

SUPPORTED_CLOSE_NAMES = {
    "tiered_tp_atr",
    "tiered_tp_atr_live",
    "tiered_tp_atr_regime",
    "tiered_tp_atr_live_regime",
    "trailing_tp_ratchet",
    "trailing_tp_ratchet_regime",
}
UNSUPPORTED_CLOSE_NAMES = {"tiered_tp_atr_live_regime_dynamic"}

STOP_REASON_PREFIXES = (
    "sl",
    "signal_sl",
    "atr_stop",
    "stop_loss",
    "trailing_stop",
)


def _round_or_none(v, places: int = 4):
    if v is None:
        return None
    return round(float(v), places)


def parse_csv(raw: str | Iterable[str] | None, *, allowed: Optional[set[str]] = None) -> list[str]:
    if raw is None:
        return []
    if isinstance(raw, str):
        items = [p.strip() for p in raw.split(",")]
    else:
        items = [str(p).strip() for p in raw]
    out = [p for p in items if p]
    if allowed is not None:
        bad = sorted(set(out) - allowed)
        if bad:
            raise ValueError(f"unknown value(s) {bad}; allowed: {sorted(allowed)}")
    return out


def _has_regime_sl_after(obj) -> bool:
    """Return true when a close params tree contains regime-aware sl_after.

    Backtester currently rejects regime-aware sl_after parity, so this harness
    fails closed before a run can produce deceptively partial economic evidence.
    """
    if isinstance(obj, dict):
        sl_after = obj.get("sl_after")
        if isinstance(sl_after, dict) and "trend_regime" in sl_after:
            return True
        return any(_has_regime_sl_after(v) for v in obj.values())
    if isinstance(obj, list):
        return any(_has_regime_sl_after(v) for v in obj)
    return False


def validate_arm_config(arm: dict) -> None:
    for ref in arm.get("close_strategies") or []:
        name = str(ref.get("name") or "").strip()
        if name in UNSUPPORTED_CLOSE_NAMES:
            raise ValueError(f"{name} is HL-live-only and not supported by #1081")
        if name and name not in SUPPORTED_CLOSE_NAMES:
            raise ValueError(
                f"close strategy {name!r} is outside the #1081 ATR surface set"
            )
        if _has_regime_sl_after(ref.get("params") or {}):
            raise ValueError("regime-aware sl_after is not backtestable in #1081")


def _arm(
    label: str,
    *,
    close_strategies=None,
    stop_loss_atr_mult=None,
    trailing_stop_atr_mult=None,
    stop_loss_atr_regime=None,
    trailing_stop_atr_regime=None,
) -> dict:
    return {
        "label": label,
        "close_strategies": deepcopy(close_strategies or []),
        "stop_loss_atr_mult": stop_loss_atr_mult,
        "trailing_stop_atr_mult": trailing_stop_atr_mult,
        "stop_loss_atr_regime": deepcopy(stop_loss_atr_regime),
        "trailing_stop_atr_regime": deepcopy(trailing_stop_atr_regime),
    }


def surface_arms(surface: str) -> tuple[dict, dict]:
    if surface == "fixed_sl":
        return (
            _arm("flat_sl_atr=2", stop_loss_atr_mult=2.0),
            _arm("regime_sl_defaults", stop_loss_atr_regime={"use_defaults": True}),
        )
    if surface == "trailing_stop":
        return (
            _arm("flat_trail_atr=3", trailing_stop_atr_mult=3.0),
            _arm("regime_trail_defaults", trailing_stop_atr_regime={"use_defaults": True}),
        )
    if surface == "tiered_tp":
        return (
            _arm(
                "flat_tiered_tp_atr_default",
                close_strategies=[{
                    "name": "tiered_tp_atr",
                    "params": {"tp_tiers": deepcopy(DEFAULT_TP_TIERS)},
                }],
            ),
            _arm(
                "regime_tiered_tp_atr_defaults",
                close_strategies=[{
                    "name": "tiered_tp_atr_regime",
                    "params": {"use_defaults": True},
                }],
            ),
        )
    if surface == "tiered_tp_live":
        return (
            _arm(
                "flat_tiered_tp_atr_live_default",
                close_strategies=[{
                    "name": "tiered_tp_atr_live",
                    "params": {
                        "atr_source": "live",
                        "tp_tiers": deepcopy(DEFAULT_TP_TIERS),
                    },
                }],
            ),
            _arm(
                "regime_tiered_tp_atr_live_defaults",
                close_strategies=[{
                    "name": "tiered_tp_atr_live_regime",
                    "params": {"use_defaults": True, "atr_source": "live"},
                }],
            ),
        )
    if surface == "trailing_ratchet":
        return (
            _arm(
                "flat_trailing_ratchet_defaults",
                close_strategies=[{
                    "name": "trailing_tp_ratchet",
                    "params": {"use_defaults": True},
                }],
                trailing_stop_atr_mult=1.5,
            ),
            _arm(
                "regime_trailing_ratchet_defaults",
                close_strategies=[{
                    "name": "trailing_tp_ratchet_regime",
                    "params": {"use_defaults": True},
                }],
                trailing_stop_atr_regime={"use_defaults": True},
            ),
        )
    raise ValueError(f"unknown surface {surface!r}; allowed: {SURFACES}")


def load_model_json(path: str) -> dict:
    with open(path) as fh:
        loaded = json.load(fh)
    if isinstance(loaded, dict) and isinstance(loaded.get("model"), dict):
        return loaded["model"]
    if not isinstance(loaded, dict):
        raise ValueError(f"{path}: model JSON must decode to an object")
    return loaded


def handrule_labels(df, *, period: int = DEFAULT_PERIOD) -> tuple[np.ndarray, np.ndarray]:
    th = dict(_DEFAULT_COMPOSITE_THRESHOLDS)
    labels = compute_regime_composite(df, period=period, thresholds=th)["regime"].to_numpy()
    feats = composite_feature_matrix(df, period, th).to_numpy()
    valid = ~np.isnan(feats).any(axis=1)
    return np.asarray(labels, dtype=object), valid


def model_labels(df, model: dict) -> tuple[np.ndarray, np.ndarray]:
    period = int(model.get("period") or DEFAULT_PERIOD)
    th = dict(_DEFAULT_COMPOSITE_THRESHOLDS)
    feats = composite_feature_matrix(df, period, th).to_numpy()
    labels, _conf = forward_filter_labels(feats, model)
    valid = ~np.isnan(feats).any(axis=1)
    return np.asarray(labels, dtype=object), valid


def validate_label_stream(labels, valid, *, source: str, min_active_labels: int = 2) -> dict:
    labels = np.asarray(labels, dtype=object)
    valid = np.asarray(valid, dtype=bool)
    if len(labels) != len(valid):
        raise ValueError(
            f"{source}: labels length {len(labels)} != valid mask length {len(valid)}"
        )
    usable = [str(x).strip() for x in labels[valid] if str(x).strip()]
    if not usable:
        raise ValueError(f"{source}: no valid regime labels to score")
    unknown = sorted(set(usable) - set(VALID_LABELS_COMPOSITE))
    if unknown:
        raise ValueError(f"{source}: unknown composite regime labels {unknown}")
    counts = OrderedDict()
    for lab in usable:
        counts[lab] = counts.get(lab, 0) + 1
    active = len(counts)
    if active < min_active_labels:
        raise ValueError(
            f"{source}: degenerate label stream active_labels={active} "
            f"< {min_active_labels}"
        )
    total = len(usable)
    return {
        "n_valid": total,
        "active_labels": active,
        "max_occupancy": round(max(counts.values()) / total, 4),
        "counts": dict(sorted(counts.items())),
    }


def inject_regime_labels(df, labels):
    labels = np.asarray(labels, dtype=object)
    if len(labels) != len(df):
        raise ValueError(f"label count {len(labels)} != dataframe rows {len(df)}")
    out = df.copy()
    out["regime"] = [str(x or "") for x in labels]
    return out


def group_entries(trades: Iterable[dict]) -> OrderedDict[str, list[dict]]:
    groups: OrderedDict[str, list[dict]] = OrderedDict()
    for trade in trades or []:
        key = str(trade.get("entry_date", ""))
        groups.setdefault(key, []).append(trade)
    return groups


def is_stop_out_reason(reason: str, prefixes: Iterable[str] = STOP_REASON_PREFIXES) -> bool:
    r = str(reason or "").strip()
    if not r:
        return False
    return any(r == p or r.startswith(p + ":") or r.startswith(p + "_") for p in prefixes)


def summarize_results(results: dict) -> dict:
    trades = list(results.get("trades", []) or [])
    entries = group_entries(trades)
    stop_entries = 0
    mae_by_entry = []
    mfe_by_entry = []
    for legs in entries.values():
        reasons = [str(t.get("exit_reason", "") or "") for t in legs]
        if any(is_stop_out_reason(r) for r in reasons):
            stop_entries += 1
        maes = [float(t.get("mae_pct", 0.0) or 0.0) for t in legs]
        mfes = [float(t.get("mfe_pct", 0.0) or 0.0) for t in legs]
        if maes:
            mae_by_entry.append(min(maes))
        if mfes:
            mfe_by_entry.append(max(mfes))

    entry_count = len(entries)
    stop_rate = (stop_entries / entry_count) if entry_count else None
    max_dd = float(results.get("max_drawdown_pct", 0.0) or 0.0)
    ret = float(results.get("total_return_pct", 0.0) or 0.0)
    out = {
        "total_return_pct": _round_or_none(ret, 4),
        "max_drawdown_pct": _round_or_none(max_dd, 4),
        "sharpe": _round_or_none(results.get("sharpe_ratio"), 4),
        "ddadj": _round_or_none(dd_adjusted_return(ret, max_dd), 4),
        "total_trades": int(results.get("total_trades", len(trades)) or 0),
        "entries": entry_count,
        "stop_outs": stop_entries,
        "stop_out_rate": _round_or_none(stop_rate, 4),
        "median_mae_pct": None,
        "p10_mae_pct": None,
        "median_mfe_pct": None,
        "exit_reasons": {},
        "liquidated": bool(results.get("liquidated")),
    }
    if mae_by_entry:
        out["median_mae_pct"] = _round_or_none(float(np.median(mae_by_entry)), 4)
        out["p10_mae_pct"] = _round_or_none(float(np.percentile(mae_by_entry, 10)), 4)
    if mfe_by_entry:
        out["median_mfe_pct"] = _round_or_none(float(np.median(mfe_by_entry)), 4)

    reasons = {}
    for t in trades:
        reason = str(t.get("exit_reason", "") or "")
        reasons[reason] = reasons.get(reason, 0) + 1
    out["exit_reasons"] = dict(sorted(reasons.items()))
    return out


def compare_summaries(control: dict, candidate: dict, *, min_sharpe_delta: float = 0.0,
                      min_ddadj_delta: float = 0.0, max_stop_rate_delta: float = 0.0,
                      min_mae_delta: float = 0.0) -> dict:
    reasons = []
    if control.get("entries", 0) <= 0:
        reasons.append("control produced no entries")
    if candidate.get("entries", 0) <= 0:
        reasons.append("candidate produced no entries")

    def _delta(key):
        a = candidate.get(key)
        b = control.get(key)
        if a is None or b is None:
            return None
        return float(a) - float(b)

    deltas = {
        "sharpe": _delta("sharpe"),
        "ddadj": _delta("ddadj"),
        "stop_out_rate": _delta("stop_out_rate"),
        "median_mae_pct": _delta("median_mae_pct"),
        "total_return_pct": _delta("total_return_pct"),
        "max_drawdown_pct": _delta("max_drawdown_pct"),
    }
    if deltas["sharpe"] is None or deltas["sharpe"] <= min_sharpe_delta:
        reasons.append("candidate Sharpe does not beat flat control")
    if deltas["ddadj"] is None or deltas["ddadj"] <= min_ddadj_delta:
        reasons.append("candidate DD-adjusted return does not beat flat control")
    if deltas["stop_out_rate"] is None:
        reasons.append("stop-out rate unavailable")
    elif deltas["stop_out_rate"] > max_stop_rate_delta:
        reasons.append("candidate stop-out rate is worse than flat control")
    # MAE is signed: closer to zero is less adverse, so candidate-control should
    # be non-negative unless the caller intentionally allows a tolerance.
    if deltas["median_mae_pct"] is None:
        reasons.append("MAE telemetry unavailable")
    elif deltas["median_mae_pct"] < min_mae_delta:
        reasons.append("candidate median MAE is worse than flat control")

    return {
        "pass": not reasons,
        "deltas": {k: _round_or_none(v, 4) for k, v in deltas.items()},
        "blocking_reasons": reasons,
    }


def _backtester_kwargs(arm: dict, *, capital: float, platform: str, strategy: str,
                       params: dict, direction: str) -> dict:
    validate_arm_config(arm)
    return {
        "initial_capital": capital,
        "platform": platform,
        "open_strategy": {"name": strategy, "params": dict(params or {})},
        "close_strategies": deepcopy(arm.get("close_strategies") or []),
        "direction": direction,
        "strategy_type": "perps",
        "regime_enabled": True,
        "stop_loss_atr_mult": arm.get("stop_loss_atr_mult"),
        "trailing_stop_atr_mult": arm.get("trailing_stop_atr_mult"),
        "stop_loss_atr_regime": deepcopy(arm.get("stop_loss_atr_regime")),
        "trailing_stop_atr_regime": deepcopy(arm.get("trailing_stop_atr_regime")),
    }


def run_arm(frame, arm: dict, *, capital: float, platform: str, strategy: str,
            params: dict, direction: str, symbol: str, timeframe: str) -> dict:
    bt = Backtester(**_backtester_kwargs(
        arm, capital=capital, platform=platform, strategy=strategy,
        params=params, direction=direction,
    ))
    return bt.run(
        frame,
        strategy_name=strategy,
        symbol=symbol,
        timeframe=timeframe,
        params=params,
        save=False,
    )


def _strategy_frame(registry: str, strategy: str, df, params: Optional[dict]):
    reg = load_registry(registry)
    spec = reg.STRATEGY_REGISTRY.get(strategy)
    if spec is None:
        raise ValueError(f"unknown strategy {strategy!r} in registry {registry!r}")
    strategy_params = dict(params if params is not None else spec["default_params"])
    frame = reg.apply_strategy(strategy, df, strategy_params)
    # Every #1081 surface is ATR-sized. Injecting ATR here prevents no-op stops
    # for open strategies that do not emit an atr column themselves.
    frame = ensure_atr_indicator(frame)
    return frame, strategy_params


def run_cell(df, labels, *, surface: str, registry: str, strategy: str,
             params: Optional[dict], capital: float, platform: str,
             direction: str, symbol: str, timeframe: str,
             thresholds: dict) -> dict:
    control_arm, candidate_arm = surface_arms(surface)
    base_frame, strategy_params = _strategy_frame(registry, strategy, df, params)
    frame = inject_regime_labels(base_frame, labels)
    control = summarize_results(run_arm(
        frame, control_arm, capital=capital, platform=platform, strategy=strategy,
        params=strategy_params, direction=direction, symbol=symbol,
        timeframe=timeframe,
    ))
    candidate = summarize_results(run_arm(
        frame, candidate_arm, capital=capital, platform=platform, strategy=strategy,
        params=strategy_params, direction=direction, symbol=symbol,
        timeframe=timeframe,
    ))
    verdict = compare_summaries(control, candidate, **thresholds)
    return {
        "surface": surface,
        "control_label": control_arm["label"],
        "candidate_label": candidate_arm["label"],
        "control": control,
        "candidate": candidate,
        "verdict": verdict,
    }


def label_sources_for(df, *, model: Optional[dict], period: int) -> OrderedDict[str, tuple[np.ndarray, np.ndarray]]:
    sources: OrderedDict[str, tuple[np.ndarray, np.ndarray]] = OrderedDict()
    sources["handrule"] = handrule_labels(df, period=period)
    if model is not None:
        sources["model"] = model_labels(df, model)
    return sources


def run_gate(
    *,
    symbol: str = DEFAULT_SYMBOL,
    timeframe: str = DEFAULT_TIMEFRAME,
    strategy: str = DEFAULT_STRATEGY,
    registry: str = DEFAULT_REGISTRY,
    windows: Iterable[str] = DEFAULT_WINDOWS,
    gate_windows: Iterable[str] = DEFAULT_GATE_WINDOWS,
    surfaces: Iterable[str] = SURFACES,
    model: Optional[dict] = None,
    period: int = DEFAULT_PERIOD,
    capital: float = DEFAULT_CAPITAL,
    platform: str = PLATFORM,
    direction: str = DEFAULT_DIRECTION,
    params: Optional[dict] = None,
    thresholds: Optional[dict] = None,
) -> dict:
    windows = list(windows)
    gate_windows = set(gate_windows)
    surfaces = list(surfaces)
    thresholds = dict(thresholds or {})
    rows = []
    label_stats = {}

    for window in windows:
        start, end = WINDOWS[window]
        df = load_cached_data(
            symbol, timeframe, exchange_id=platform, start_date=start, end_date=end,
        )
        if df.empty:
            rows.append({
                "window": window,
                "error": "no cached data",
                "verdict": {"pass": False, "blocking_reasons": ["no cached data"]},
            })
            continue

        for source, (labels, valid) in label_sources_for(df, model=model, period=period).items():
            stat_key = f"{source}:{window}"
            try:
                label_stats[stat_key] = validate_label_stream(labels, valid, source=stat_key)
            except ValueError as exc:
                rows.append({
                    "window": window,
                    "label_source": source,
                    "error": str(exc),
                    "verdict": {"pass": False, "blocking_reasons": [str(exc)]},
                })
                continue
            for surface in surfaces:
                try:
                    cell = run_cell(
                        df, labels, surface=surface, registry=registry,
                        strategy=strategy, params=params, capital=capital,
                        platform=platform, direction=direction, symbol=symbol,
                        timeframe=timeframe, thresholds=thresholds,
                    )
                    cell.update({"window": window, "label_source": source})
                except Exception as exc:  # fail closed, report the blocked cell
                    cell = {
                        "window": window,
                        "label_source": source,
                        "surface": surface,
                        "error": f"{type(exc).__name__}: {exc}",
                        "verdict": {
                            "pass": False,
                            "blocking_reasons": [f"{type(exc).__name__}: {exc}"],
                        },
                    }
                rows.append(cell)

    gating_rows = [
        r for r in rows
        if r.get("window") in gate_windows
        and "verdict" in r
    ]
    blocking = []
    for r in gating_rows:
        if not r.get("verdict", {}).get("pass"):
            prefix = f"{r.get('label_source', '-')}/{r.get('surface', '-')}/{r.get('window', '-')}"
            for reason in r.get("verdict", {}).get("blocking_reasons", []):
                blocking.append(f"{prefix}: {reason}")
    if not gating_rows:
        blocking.append("no gate-window rows were evaluated")

    return {
        "issue": 1081,
        "symbol": symbol,
        "timeframe": timeframe,
        "strategy": strategy,
        "registry": registry,
        "platform": platform,
        "direction": direction,
        "windows": windows,
        "gate_windows": sorted(gate_windows),
        "surfaces": surfaces,
        "period": int(period),
        "thresholds": thresholds,
        "label_stats": label_stats,
        "rows": rows,
        "summary": {
            "pass": not blocking,
            "blocking_reasons": sorted(blocking),
        },
    }


def format_report(report: dict) -> str:
    lines = []
    lines.append("=" * 100)
    lines.append(
        "REGIME ATR ECONOMIC GATE (#1081) "
        f"{report['symbol']} {report['timeframe']} strategy={report['strategy']}"
    )
    lines.append("=" * 100)
    hdr = (
        f"{'src':8s} {'surface':18s} {'win':7s} | "
        f"{'c_sh':>7s} {'r_sh':>7s} {'d_sh':>7s} | "
        f"{'c_dda':>7s} {'r_dda':>7s} {'d_dda':>7s} | "
        f"{'c_stop':>7s} {'r_stop':>7s} | {'c_mae':>7s} {'r_mae':>7s} | pass"
    )
    lines.append(hdr)
    lines.append("-" * len(hdr))
    for row in report.get("rows", []):
        verdict = row.get("verdict", {})
        if "control" not in row or "candidate" not in row:
            lines.append(
                f"{row.get('label_source', '-')[:8]:8s} "
                f"{row.get('surface', '-')[:18]:18s} {row.get('window', '-')[:7]:7s} | "
                f"ERROR {row.get('error', '')}"
            )
            continue
        c = row["control"]
        r = row["candidate"]
        d = verdict.get("deltas", {})

        def fmt(v):
            return "      -" if v is None else f"{float(v):7.2f}"

        lines.append(
            f"{row['label_source'][:8]:8s} {row['surface'][:18]:18s} "
            f"{row['window'][:7]:7s} | "
            f"{fmt(c.get('sharpe'))} {fmt(r.get('sharpe'))} {fmt(d.get('sharpe'))} | "
            f"{fmt(c.get('ddadj'))} {fmt(r.get('ddadj'))} {fmt(d.get('ddadj'))} | "
            f"{fmt(c.get('stop_out_rate'))} {fmt(r.get('stop_out_rate'))} | "
            f"{fmt(c.get('median_mae_pct'))} {fmt(r.get('median_mae_pct'))} | "
            f"{'Y' if verdict.get('pass') else '.'}"
        )
    lines.append("")
    lines.append("SUMMARY")
    lines.append(f"  pass: {bool(report.get('summary', {}).get('pass'))}")
    for reason in report.get("summary", {}).get("blocking_reasons", []):
        lines.append(f"  block: {reason}")
    return "\n".join(lines)


def build_parser() -> argparse.ArgumentParser:
    p = argparse.ArgumentParser(description="#1081 economic ATR regime gate")
    p.add_argument("--symbol", default=DEFAULT_SYMBOL)
    p.add_argument("--timeframe", default=DEFAULT_TIMEFRAME)
    p.add_argument("--strategy", default=DEFAULT_STRATEGY)
    p.add_argument("--registry", default=DEFAULT_REGISTRY, choices=["spot", "futures"])
    p.add_argument("--direction", default=DEFAULT_DIRECTION, choices=["long", "short", "both"])
    p.add_argument("--windows", default=",".join(DEFAULT_WINDOWS))
    p.add_argument("--gate-windows", default=",".join(DEFAULT_GATE_WINDOWS))
    p.add_argument("--surfaces", default=",".join(SURFACES),
                   help=f"comma list from {','.join(SURFACES)}")
    p.add_argument("--model-json", default=None,
                   help="optional #1080 model JSON; may be raw model or {'model': ...}")
    p.add_argument("--period", type=int, default=DEFAULT_PERIOD)
    p.add_argument("--capital", type=float, default=DEFAULT_CAPITAL)
    p.add_argument("--platform", default=PLATFORM)
    p.add_argument("--params", default=None, help="JSON object of open-strategy params")
    p.add_argument("--json", default=None, help="write machine-readable report here")
    p.add_argument("--min-sharpe-delta", type=float, default=0.0)
    p.add_argument("--min-ddadj-delta", type=float, default=0.0)
    p.add_argument("--max-stop-rate-delta", type=float, default=0.0)
    p.add_argument("--min-mae-delta", type=float, default=0.0)
    return p


def main(argv=None) -> int:
    args = build_parser().parse_args(argv)
    windows = parse_csv(args.windows, allowed=set(WINDOWS))
    gate_windows = parse_csv(args.gate_windows, allowed=set(WINDOWS))
    surfaces = parse_csv(args.surfaces, allowed=set(SURFACES))
    params = json.loads(args.params) if args.params else None
    if params is not None and not isinstance(params, dict):
        raise SystemExit("--params must decode to a JSON object")
    model = load_model_json(args.model_json) if args.model_json else None
    report = run_gate(
        symbol=args.symbol,
        timeframe=args.timeframe,
        strategy=args.strategy,
        registry=args.registry,
        windows=windows,
        gate_windows=gate_windows,
        surfaces=surfaces,
        model=model,
        period=args.period,
        capital=args.capital,
        platform=args.platform,
        direction=args.direction,
        params=params,
        thresholds={
            "min_sharpe_delta": args.min_sharpe_delta,
            "min_ddadj_delta": args.min_ddadj_delta,
            "max_stop_rate_delta": args.max_stop_rate_delta,
            "min_mae_delta": args.min_mae_delta,
        },
    )
    if args.json:
        with open(args.json, "w") as fh:
            json.dump(report, fh, indent=2, default=float)
            fh.write("\n")
    print(format_report(report))
    return 0 if report["summary"]["pass"] else 1


if __name__ == "__main__":
    raise SystemExit(main())

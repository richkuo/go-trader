#!/usr/bin/env python3
"""Backtest-vs-paper parity trace helper.

This tool covers the high-leverage audit need from #906: produce a per-bar
backtest decision trace and optionally diff it against a paper/live trace CSV.
It intentionally compares normalized trace columns instead of trying to
recreate scheduler state from logs.
"""

from __future__ import annotations

import argparse
import json
import os
import sys
from typing import Iterable, Optional

import pandas as pd

_BACKTEST_DIR = os.path.abspath(os.path.dirname(__file__))
_REPO_ROOT = os.path.abspath(os.path.join(_BACKTEST_DIR, ".."))
if _BACKTEST_DIR not in sys.path:
    sys.path.insert(0, _BACKTEST_DIR)
if os.path.join(_REPO_ROOT, "shared_tools") not in sys.path:
    sys.path.insert(0, os.path.join(_REPO_ROOT, "shared_tools"))

from atr import ensure_atr_indicator  # noqa: E402
from backtester import Backtester  # noqa: E402
from data_fetcher import load_cached_data  # noqa: E402
from registry_loader import load_registry  # noqa: E402
from run_backtest import (  # noqa: E402
    REGIME_ALLOWED_LABELS,
    _parse_regime_thresholds_json,
    _parse_regime_windows_spec_arg,
    load_strategy_config,
)


TRACE_COLUMNS = [
    "date",
    "signal",
    "regime",
    "market_regime",
    "open_action",
    "close_fraction",
    "scheduled_close_fraction",
    "fill_px",
    "fee",
    "event",
    "position_after",
    "equity_after",
]

DEFAULT_COMPARE_COLUMNS = [
    "signal",
    "regime",
    "open_action",
    "close_fraction",
    "fill_px",
    "fee",
]


def _parse_json_obj(raw: Optional[str], flag: str) -> dict:
    if raw is None or str(raw).strip() == "":
        return {}
    try:
        parsed = json.loads(raw)
    except json.JSONDecodeError as exc:
        raise SystemExit(f"{flag} is not valid JSON: {exc}\nGot: {raw}")
    if not isinstance(parsed, dict):
        raise SystemExit(f"{flag} must be a JSON object, got {type(parsed).__name__}")
    return parsed


def _filter_until(df: pd.DataFrame, until: Optional[str]) -> pd.DataFrame:
    if not until:
        return df
    cutoff = pd.Timestamp(until)
    return df.loc[df.index <= cutoff]


def trace_to_frame(result: dict) -> pd.DataFrame:
    rows = result.get("debug_trace") or []
    frame = pd.DataFrame(rows)
    for col in TRACE_COLUMNS:
        if col not in frame.columns:
            frame[col] = "" if col in {"date", "regime", "market_regime", "open_action", "event"} else 0.0
    return frame[TRACE_COLUMNS]


def build_backtest_trace(
    *,
    strategy: str,
    symbol: str,
    timeframe: str,
    since: str,
    until: Optional[str] = None,
    capital: float = 1000.0,
    registry: str = "spot",
    platform: str = "binanceus",
    params: Optional[dict] = None,
    close_strategies: Optional[list[dict]] = None,
    regime_enabled: bool = False,
    regime_period: int = 14,
    regime_adx_threshold: float = 20.0,
    regime_classifier: str = "adx",
    regime_thresholds: Optional[dict] = None,
    regime_windows_spec: Optional[dict] = None,
    regime_gate_window: str = "",
    allowed_regimes: Optional[list[str]] = None,
    stop_loss_atr_mult: Optional[float] = None,
    stop_loss_pct: Optional[float] = None,
    stop_loss_margin_pct: Optional[float] = None,
    trailing_stop_atr_mult: Optional[float] = None,
    trailing_stop_pct: Optional[float] = None,
    stop_loss_atr_regime: Optional[dict] = None,
    trailing_stop_atr_regime: Optional[dict] = None,
    strategy_type: str = "perps",
) -> pd.DataFrame:
    reg = load_registry(registry)
    entry = reg.STRATEGY_REGISTRY.get(strategy)
    if not entry:
        raise SystemExit(
            f"Unknown strategy {strategy!r} in {registry!r} registry. "
            f"Available: {reg.list_strategies()}"
        )

    strat_params = params if params is not None else entry["default_params"]
    df = load_cached_data(symbol, timeframe, start_date=since)
    df = _filter_until(df, until)
    if df.empty:
        raise SystemExit(
            f"No cached data for {symbol} {timeframe} from {since}"
            + (f" through {until}" if until else "")
        )

    signals = reg.apply_strategy(strategy, df, dict(strat_params or {}))
    if close_strategies:
        signals = ensure_atr_indicator(signals)

    bt = Backtester(
        initial_capital=capital,
        platform=platform,
        open_strategy={"name": strategy, "params": dict(strat_params or {})},
        close_strategies=close_strategies,
        regime_enabled=regime_enabled,
        regime_period=regime_period,
        regime_adx_threshold=regime_adx_threshold,
        regime_classifier=regime_classifier,
        regime_thresholds=regime_thresholds,
        regime_windows_spec=regime_windows_spec,
        regime_gate_window=regime_gate_window,
        allowed_regimes=allowed_regimes,
        stop_loss_atr_mult=stop_loss_atr_mult,
        stop_loss_pct=stop_loss_pct,
        stop_loss_margin_pct=stop_loss_margin_pct,
        trailing_stop_atr_mult=trailing_stop_atr_mult,
        trailing_stop_pct=trailing_stop_pct,
        stop_loss_atr_regime=stop_loss_atr_regime,
        trailing_stop_atr_regime=trailing_stop_atr_regime,
        strategy_type=strategy_type,
    )
    result = bt.run(
        signals,
        strategy_name=strategy,
        symbol=symbol,
        timeframe=timeframe,
        params=dict(strat_params or {}),
        save=False,
        trace=True,
    )
    return trace_to_frame(result)


def _normalize_date_col(df: pd.DataFrame) -> pd.DataFrame:
    out = df.copy()
    if "date" not in out.columns:
        raise ValueError("trace CSV must include a date column")
    out["date"] = pd.to_datetime(out["date"]).astype(str)
    return out


def _is_number(value) -> bool:
    try:
        float(value)
        return True
    except (TypeError, ValueError):
        return False


def compare_traces(
    backtest: pd.DataFrame,
    paper: pd.DataFrame,
    *,
    columns: Iterable[str] = DEFAULT_COMPARE_COLUMNS,
    tolerance: float = 1e-8,
) -> pd.DataFrame:
    left = _normalize_date_col(backtest)
    right = _normalize_date_col(paper)
    cols = [c for c in columns if c in left.columns or c in right.columns]
    merged = left[["date"] + [c for c in cols if c in left.columns]].merge(
        right[["date"] + [c for c in cols if c in right.columns]],
        on="date",
        how="outer",
        suffixes=("_backtest", "_paper"),
        indicator=True,
    )

    diffs: list[dict] = []
    for _, row in merged.sort_values("date").iterrows():
        date = row["date"]
        if row["_merge"] != "both":
            diffs.append({
                "date": date,
                "column": "_row",
                "backtest": "present" if row["_merge"] == "left_only" else "missing",
                "paper": "present" if row["_merge"] == "right_only" else "missing",
                "delta": "",
            })
            continue
        for col in cols:
            lcol = f"{col}_backtest"
            rcol = f"{col}_paper"
            b = row[lcol] if lcol in row else ""
            p = row[rcol] if rcol in row else ""
            if pd.isna(b):
                b = ""
            if pd.isna(p):
                p = ""
            delta = ""
            if _is_number(b) and _is_number(p):
                delta_f = float(b) - float(p)
                if abs(delta_f) <= tolerance:
                    continue
                delta = delta_f
            elif str(b) == str(p):
                continue
            diffs.append({
                "date": date,
                "column": col,
                "backtest": b,
                "paper": p,
                "delta": delta,
            })
    return pd.DataFrame(diffs, columns=["date", "column", "backtest", "paper", "delta"])


def _write_frame(df: pd.DataFrame, path: Optional[str]) -> None:
    if path:
        os.makedirs(os.path.dirname(path) or ".", exist_ok=True)
        df.to_csv(path, index=False)
    else:
        print(df.to_csv(index=False), end="")


def _build_parser() -> argparse.ArgumentParser:
    p = argparse.ArgumentParser(description="Backtest-vs-paper parity trace helper")
    p.add_argument("--config", help="Live go-trader config.json to load")
    p.add_argument("--strategy-id", help="Strategy id inside --config")
    p.add_argument("--strategy", help="Direct open strategy name (without --config)")
    p.add_argument("--params-json", help="Direct strategy params JSON")
    p.add_argument("--registry", choices=["spot", "futures"], default="spot")
    p.add_argument("--platform", default="binanceus")
    p.add_argument("--symbol", required=True)
    p.add_argument("--timeframe", required=True)
    p.add_argument("--since", required=True)
    p.add_argument("--until")
    p.add_argument("--capital", type=float, default=1000.0)
    p.add_argument("--defaults", choices=["system", "user"], default="system")
    p.add_argument("--regime-enabled", action="store_true")
    p.add_argument("--regime-period", type=int, default=14)
    p.add_argument("--regime-adx-threshold", type=float, default=20.0)
    p.add_argument("--regime-classifier", choices=["adx", "composite"], default="adx")
    p.add_argument("--regime-thresholds-json")
    p.add_argument("--regime-windows-spec-json")
    p.add_argument("--regime-gate-window", default="")
    p.add_argument("--allowed-regimes", action="append", choices=REGIME_ALLOWED_LABELS)
    p.add_argument("--trace-out", help="Write generated backtest trace CSV")
    p.add_argument("--paper-trace", help="Paper/live trace CSV to compare")
    p.add_argument("--diff-out", help="Write diff CSV when --paper-trace is set")
    p.add_argument("--compare-columns", default=",".join(DEFAULT_COMPARE_COLUMNS))
    p.add_argument("--tolerance", type=float, default=1e-8)
    return p


def main() -> int:
    args = _build_parser().parse_args()
    params = _parse_json_obj(args.params_json, "--params-json")
    close_refs = None
    stop_kwargs: dict = {}
    strategy = args.strategy

    if args.config:
        if not args.strategy_id:
            raise SystemExit("--strategy-id is required with --config")
        loaded = load_strategy_config(
            args.config,
            args.strategy_id,
            inject_user_defaults=(args.defaults == "user"),
        )
        strategy = loaded["open_strategy"]["name"]
        params = loaded["open_strategy"]["params"]
        close_refs = loaded["close_strategies"]
        stop_keys = (
            "stop_loss_atr_mult",
            "stop_loss_pct",
            "stop_loss_margin_pct",
            "trailing_stop_atr_mult",
            "trailing_stop_pct",
            "stop_loss_atr_regime",
            "trailing_stop_atr_regime",
            "strategy_type",
        )
        stop_kwargs = {k: loaded[k] for k in stop_keys if k in loaded}
    elif not strategy:
        raise SystemExit("Provide either --config + --strategy-id, or --strategy")

    trace_df = build_backtest_trace(
        strategy=strategy,
        symbol=args.symbol,
        timeframe=args.timeframe,
        since=args.since,
        until=args.until,
        capital=args.capital,
        registry=args.registry,
        platform=args.platform,
        params=params,
        close_strategies=close_refs,
        regime_enabled=args.regime_enabled,
        regime_period=args.regime_period,
        regime_adx_threshold=args.regime_adx_threshold,
        regime_classifier=args.regime_classifier,
        regime_thresholds=_parse_regime_thresholds_json(args.regime_thresholds_json),
        regime_windows_spec=_parse_regime_windows_spec_arg(args.regime_windows_spec_json),
        regime_gate_window=args.regime_gate_window,
        allowed_regimes=args.allowed_regimes,
        **stop_kwargs,
    )
    _write_frame(trace_df, args.trace_out)

    if not args.paper_trace:
        return 0

    paper_df = pd.read_csv(args.paper_trace)
    columns = [c.strip() for c in args.compare_columns.split(",") if c.strip()]
    diff_df = compare_traces(
        trace_df,
        paper_df,
        columns=columns,
        tolerance=args.tolerance,
    )
    _write_frame(diff_df, args.diff_out)
    return 1 if not diff_df.empty else 0


if __name__ == "__main__":
    raise SystemExit(main())

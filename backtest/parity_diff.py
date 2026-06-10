#!/usr/bin/env python3
"""
Backtest vs live decision parity diff (#906 D7.4).

Compares per-bar strategy decisions between:
  - **vector** — full-history ``apply_strategy`` (what the backtester ingests)
  - **live** — per-bar sliding-window replay using check-script semantics
    (``prepare_check_regime`` + ``evaluate_open_close`` / last-bar signal)

Also reports **backtest_effective** columns (post-``shift(1)`` inputs the engine
reads at each bar) and optional simulated **fills** (entry/exit price + fee)
from ``Backtester.run``.

Usage::

    python backtest/parity_diff.py sma_crossover BTC/USDT 1d \\
        --since 2024-01-01 --limit 120

    python backtest/parity_diff.py --config config.json --strategy-id btc-4h \\
        --since 2024-01-01 --output diff.jsonl

Exit code 0 when vector/live decision columns match on every compared bar;
exit 1 when any mismatch is found (or on fatal error).
"""

from __future__ import annotations

import argparse
import json
import math
import os
import sys
from dataclasses import dataclass, field
from typing import Any, Callable, Iterable, Optional

import numpy as np
import pandas as pd

_ROOT = os.path.abspath(os.path.join(os.path.dirname(__file__), ".."))
_TOOLS = os.path.join(_ROOT, "shared_tools")
_BACKTEST = os.path.join(_ROOT, "backtest")
for _p in (_TOOLS, _BACKTEST):
    if _p not in sys.path:
        sys.path.insert(0, _p)

from atr import ensure_atr_indicator, latest_atr  # noqa: E402
from regime import prepare_check_regime  # noqa: E402
from registry_loader import load_registry  # noqa: E402
from strategy_composition import (  # noqa: E402
    evaluate_open_close,
    finalize_decision,
    normalize_signal,
    open_action_from_signal,
    parse_close_strategies,
)
from close_registry_loader import evaluate as close_evaluate  # noqa: E402
from backtester import (  # noqa: E402
    Backtester,
    _max_close_fraction_series,
    _normalize_open_action,
    _open_action_from_signal,
    fee_pct_for_platform,
)
from run_backtest import load_strategy_config  # noqa: E402


DECISION_COLS = ("signal", "regime", "open_action", "close_fraction")


@dataclass
class ParityConfig:
    strategy_name: str
    symbol: str
    timeframe: str
    params: dict
    registry: str = "spot"
    platform: str = "binanceus"
    open_strategy: Optional[str] = None
    close_strategies: Optional[list[dict]] = None
    regime_enabled: bool = False
    regime_period: int = 14
    regime_adx_threshold: float = 20.0
    min_warmup: int = 30


@dataclass
class ParityDiffResult:
    rows: list[dict] = field(default_factory=list)
    mismatches: int = 0
    bars_compared: int = 0
    fills: list[dict] = field(default_factory=list)


def _iso(ts) -> str:
    if ts is None or (isinstance(ts, float) and math.isnan(ts)):
        return ""
    stamp = pd.Timestamp(ts)
    if stamp.tzinfo is None:
        stamp = stamp.tz_localize("UTC")
    else:
        stamp = stamp.tz_convert("UTC")
    return stamp.isoformat()


def _close_names(close_strategies: Optional[list[dict]]) -> list[str]:
    if not close_strategies:
        return []
    return [str(r.get("name", "")).strip() for r in close_strategies if r.get("name")]


def _close_params_by_name(close_strategies: Optional[list[dict]]) -> dict[str, dict]:
    out: dict[str, dict] = {}
    if not close_strategies:
        return out
    for ref in close_strategies:
        name = str(ref.get("name", "")).strip()
        if name:
            out[name] = dict(ref.get("params") or {})
    return out


def _extract_row_decision(
    result_df: pd.DataFrame,
    idx,
    *,
    regime_label: str = "",
    decision: Optional[dict] = None,
) -> dict[str, Any]:
    if decision:
        return {
            "signal": int(decision.get("signal", 0) or 0),
            "regime": str(regime_label or decision.get("regime") or ""),
            "open_action": str(decision.get("open_action", "none") or "none"),
            "close_fraction": float(decision.get("close_fraction", 0.0) or 0.0),
        }
    if result_df.empty:
        return {"signal": 0, "regime": "", "open_action": "none", "close_fraction": 0.0}
    row = result_df.loc[idx] if idx in result_df.index else result_df.iloc[-1]
    signal = normalize_signal(row.get("signal", 0))
    regime = str(regime_label or row.get("regime", "") or "")
    if "open_action" in result_df.columns:
        open_action = _normalize_open_action(row.get("open_action"))
    else:
        open_action = _open_action_from_signal(signal)
    if "close_fraction" in result_df.columns or any(
        str(c).startswith("close_fraction:") for c in result_df.columns
    ):
        close_fraction = float(_max_close_fraction_series(result_df.loc[[idx]]).iloc[0])
    else:
        close_fraction = 0.0
    return {
        "signal": signal,
        "regime": regime,
        "open_action": open_action,
        "close_fraction": close_fraction,
    }


def live_bar_decision(
    window: pd.DataFrame,
    cfg: ParityConfig,
    *,
    apply_strategy: Callable,
    get_strategy: Callable,
    position_side: str = "",
    position_ctx: Optional[dict] = None,
) -> dict[str, Any]:
    """Mirror ``check_strategy.py`` decision for a window ending at the current bar."""
    _stdout_regime, live_regime, strategy_regime = prepare_check_regime(
        window,
        regime_enabled=cfg.regime_enabled,
        period=cfg.regime_period,
        adx_threshold=cfg.regime_adx_threshold,
    )
    params = dict(cfg.params or {})
    if cfg.regime_enabled:
        params["regime"] = strategy_regime

    open_name = (cfg.open_strategy or cfg.strategy_name).strip()
    close_names = _close_names(cfg.close_strategies)
    close_csv = ",".join(close_names) if close_names else None
    open_close_enabled = bool(cfg.open_strategy or close_names)

    if open_close_enabled:
        market_ctx: dict[str, Any] = {"mark_price": float(window["close"].iloc[-1])}
        atr_now = latest_atr(window)
        if atr_now > 0:
            market_ctx["atr"] = atr_now
        if live_regime:
            market_ctx["regime"] = live_regime
        evaluation = evaluate_open_close(
            apply_strategy,
            get_strategy,
            window,
            cfg.strategy_name,
            cfg.open_strategy,
            parse_close_strategies(close_csv),
            position_side,
            params,
            position_ctx or {},
            close_evaluate=close_evaluate,
            market_ctx=market_ctx,
            close_params_by_name=_close_params_by_name(cfg.close_strategies),
        )
        decision = finalize_decision(evaluation, position_side, evaluation.open_signal)
        regime_out = live_regime if cfg.regime_enabled else ""
        if isinstance(_stdout_regime, dict):
            regime_out = str(live_regime or "")
        elif cfg.regime_enabled:
            regime_out = str(_stdout_regime or "")
        return _extract_row_decision(
            evaluation.open_result_df,
            window.index[-1],
            regime_label=regime_out,
            decision=decision,
        )

    result_df = apply_strategy(cfg.strategy_name, window, params)
    regime_out = ""
    if cfg.regime_enabled:
        if isinstance(_stdout_regime, str):
            regime_out = _stdout_regime
        elif isinstance(strategy_regime, dict):
            regime_out = str(strategy_regime.get("regime") or "")
    return _extract_row_decision(result_df, window.index[-1], regime_label=regime_out)


def vector_frame(
    df: pd.DataFrame,
    cfg: ParityConfig,
    *,
    apply_strategy: Callable,
    ensure_regime_columns: Callable,
) -> pd.DataFrame:
    """Full-history vectorized decisions (pre-shift) at each bar."""
    work = df.copy()
    if cfg.regime_enabled and "regime" not in work.columns:
        ensure_regime_columns(
            work,
            period=cfg.regime_period,
            adx_threshold=cfg.regime_adx_threshold,
        )

    params = dict(cfg.params or {})
    if cfg.regime_enabled and "regime" in work.columns:
        params["regime"] = str(work["regime"].iloc[-1] or "")

    open_name = (cfg.open_strategy or cfg.strategy_name).strip()
    result = apply_strategy(open_name, work, params)
    out = pd.DataFrame(index=work.index)
    if "close" in work.columns:
        out["close"] = work["close"].astype(float)
    out["signal"] = result.get("signal", pd.Series(0, index=work.index)).fillna(0).map(normalize_signal)
    if "open_action" in result.columns:
        out["open_action"] = result["open_action"].map(_normalize_open_action)
    else:
        out["open_action"] = out["signal"].map(_open_action_from_signal)
    out["close_fraction"] = _max_close_fraction_series(result).reindex(work.index).fillna(0.0)
    if cfg.regime_enabled and "regime" in work.columns:
        out["regime"] = work["regime"].fillna("").astype(str)
    else:
        out["regime"] = ""
    return out


def backtest_effective_frame(vector: pd.DataFrame) -> pd.DataFrame:
    """Post-``shift(1)`` inputs the backtester reads at each bar."""
    eff = vector.copy()
    for col in ("signal", "open_action", "close_fraction", "regime"):
        if col in eff.columns:
            eff[col] = eff[col].shift(1)
    eff["signal"] = eff.get("signal", pd.Series(0, index=vector.index)).fillna(0).astype(int)
    eff["open_action"] = eff.get("open_action", "none").fillna("none")
    eff["close_fraction"] = eff.get("close_fraction", 0.0).fillna(0.0).astype(float)
    eff["regime"] = eff.get("regime", "").fillna("").astype(str)
    return eff


def _simulate_position_context(
    vector: pd.DataFrame,
    live_rows: list[dict],
) -> list[Optional[dict]]:
    """Lightweight position tracker so live close evaluators see plausible context."""
    contexts: list[Optional[dict]] = []
    side = ""
    avg_cost = 0.0
    qty = 0.0
    initial_qty = 0.0
    entry_regime = ""

    for i, idx in enumerate(vector.index):
        ctx: Optional[dict] = None
        if side:
            ctx = {
                "side": side,
                "avg_cost": avg_cost,
                "current_quantity": qty,
                "initial_quantity": initial_qty or qty,
                "regime": entry_regime,
            }
        contexts.append(ctx)

        live = live_rows[i] if i < len(live_rows) else {}
        eff_signal = int(live.get("backtest_effective_signal", 0) or 0)
        eff_close = float(live.get("backtest_effective_close_fraction", 0.0) or 0.0)
        eff_open = str(live.get("backtest_effective_open_action", "none") or "none")

        mark = float(vector.loc[idx, "close"]) if "close" in vector.columns else 0.0
        if eff_close > 0 and side:
            closed = qty * eff_close
            qty = max(qty - closed, 0.0)
            if qty <= 1e-12:
                side = ""
                avg_cost = 0.0
                qty = 0.0
                initial_qty = 0.0
                entry_regime = ""
        elif eff_open in ("long", "short") and not side:
            side = eff_open
            avg_cost = mark
            qty = 1.0
            initial_qty = 1.0
            entry_regime = str(live.get("live_regime", "") or "")
        elif eff_signal > 0 and not side:
            side = "long"
            avg_cost = mark
            qty = 1.0
            initial_qty = 1.0
            entry_regime = str(live.get("live_regime", "") or "")
        elif eff_signal < 0 and not side:
            side = "short"
            avg_cost = mark
            qty = 1.0
            initial_qty = 1.0
            entry_regime = str(live.get("live_regime", "") or "")

    return contexts


def compare_parity(
    df: pd.DataFrame,
    cfg: ParityConfig,
    *,
    apply_strategy: Callable,
    get_strategy: Callable,
    ensure_regime_columns: Callable,
    include_fills: bool = True,
) -> ParityDiffResult:
    """Build per-bar diff rows between vector and live replay paths."""
    if len(df) < cfg.min_warmup:
        raise ValueError(
            f"Need at least {cfg.min_warmup} bars for warmup; got {len(df)}"
        )

    vec = vector_frame(df, cfg, apply_strategy=apply_strategy, ensure_regime_columns=ensure_regime_columns)
    effective = backtest_effective_frame(vec)

    # Seed position contexts from backtest-effective opens/closes when close
    # evaluators need a position snapshot (mirrors check_strategy --position-*).
    scaffold: list[dict] = []
    for end in range(cfg.min_warmup, len(df)):
        idx = df.index[end]
        scaffold.append({
            "backtest_effective_signal": int(effective.loc[idx, "signal"]),
            "backtest_effective_open_action": str(effective.loc[idx, "open_action"]),
            "backtest_effective_close_fraction": float(effective.loc[idx, "close_fraction"]),
            "live_regime": str(vec.loc[idx, "regime"]),
        })
    position_contexts = (
        _simulate_position_context(vec.iloc[cfg.min_warmup:], scaffold)
        if cfg.close_strategies
        else [None] * len(scaffold)
    )

    live_rows: list[dict] = []
    for j, end in enumerate(range(cfg.min_warmup, len(df))):
        window = df.iloc[: end + 1]
        ctx = position_contexts[j] if j < len(position_contexts) else None
        side = str((ctx or {}).get("side", "") or "")
        live = live_bar_decision(
            window,
            cfg,
            apply_strategy=apply_strategy,
            get_strategy=get_strategy,
            position_side=side,
            position_ctx=ctx if side else None,
        )
        idx = df.index[end]
        row = {
            "bar": _iso(idx),
            "close": float(df["close"].iloc[end]),
            "vector_signal": int(vec.loc[idx, "signal"]),
            "live_signal": int(live["signal"]),
            "vector_regime": str(vec.loc[idx, "regime"]),
            "live_regime": str(live["regime"]),
            "vector_open_action": str(vec.loc[idx, "open_action"]),
            "live_open_action": str(live["open_action"]),
            "vector_close_fraction": float(vec.loc[idx, "close_fraction"]),
            "live_close_fraction": float(live["close_fraction"]),
            "backtest_effective_signal": int(effective.loc[idx, "signal"]),
            "backtest_effective_open_action": str(effective.loc[idx, "open_action"]),
            "backtest_effective_close_fraction": float(effective.loc[idx, "close_fraction"]),
            "backtest_effective_regime": str(effective.loc[idx, "regime"]),
        }
        row["decision_match"] = all(
            row[f"vector_{c}"] == row[f"live_{c}"] for c in DECISION_COLS
        )
        live_rows.append(row)

    result = ParityDiffResult(
        rows=live_rows,
        mismatches=sum(1 for r in live_rows if not r["decision_match"]),
        bars_compared=len(live_rows),
    )

    if include_fills:
        result.fills = _extract_fills(df, cfg, apply_strategy=apply_strategy, ensure_regime_columns=ensure_regime_columns)
    return result


def _extract_fills(
    df: pd.DataFrame,
    cfg: ParityConfig,
    *,
    apply_strategy: Callable,
    ensure_regime_columns: Callable,
) -> list[dict]:
    work = df.copy()
    open_name = (cfg.open_strategy or cfg.strategy_name).strip()
    params = dict(cfg.params or {})
    work = apply_strategy(open_name, work, params)
    if cfg.close_strategies:
        work = ensure_atr_indicator(work)
    if cfg.regime_enabled and "regime" not in work.columns:
        ensure_regime_columns(
            work,
            period=cfg.regime_period,
            adx_threshold=cfg.regime_adx_threshold,
        )

    bt = Backtester(
        initial_capital=1000.0,
        platform=cfg.platform,
        open_strategy={"name": open_name, "params": params},
        close_strategies=cfg.close_strategies,
        regime_enabled=cfg.regime_enabled,
        regime_period=cfg.regime_period,
        regime_adx_threshold=cfg.regime_adx_threshold,
    )
    metrics = bt.run(
        work,
        strategy_name=cfg.strategy_name,
        symbol=cfg.symbol,
        timeframe=cfg.timeframe,
        params=params,
        save=False,
    )
    fee_pct = fee_pct_for_platform(cfg.platform)
    fills: list[dict] = []
    for trade in metrics.get("trades", []) or []:
        entry_fee = float(trade.get("entry_price", 0) or 0) * abs(float(trade.get("shares", 0) or 0)) * fee_pct
        exit_fee = float(trade.get("exit_price", 0) or 0) * abs(float(trade.get("shares", 0) or 0)) * fee_pct
        fills.append({
            "event": "entry",
            "bar": _iso(trade.get("entry_date")),
            "side": trade.get("side", "long"),
            "fill_px": float(trade.get("entry_price", 0) or 0),
            "fee": round(entry_fee, 6),
        })
        if trade.get("exit_date"):
            fills.append({
                "event": "exit",
                "bar": _iso(trade.get("exit_date")),
                "side": trade.get("side", "long"),
                "fill_px": float(trade.get("exit_price", 0) or 0),
                "fee": round(exit_fee, 6),
                "pnl": float(trade.get("pnl", 0) or 0),
            })
    return fills


def format_summary(result: ParityDiffResult) -> str:
    lines = [
        f"Bars compared: {result.bars_compared}",
        f"Mismatches:    {result.mismatches}",
        f"Fills:         {len(result.fills)}",
    ]
    if result.mismatches:
        lines.append("")
        lines.append("First mismatches:")
        shown = 0
        for row in result.rows:
            if row.get("decision_match"):
                continue
            lines.append(
                f"  {row['bar']}: "
                f"signal {row['vector_signal']}≠{row['live_signal']} "
                f"regime {row['vector_regime']!r}≠{row['live_regime']!r} "
                f"open {row['vector_open_action']}≠{row['live_open_action']} "
                f"close {row['vector_close_fraction']}≠{row['live_close_fraction']}"
            )
            shown += 1
            if shown >= 5:
                break
    return "\n".join(lines)


def run_from_dataframe(df: pd.DataFrame, cfg: ParityConfig, **kwargs) -> ParityDiffResult:
    reg = load_registry(cfg.registry)
    from regime import ensure_regime_columns

    return compare_parity(
        df,
        cfg,
        apply_strategy=reg.apply_strategy,
        get_strategy=reg.get_strategy,
        ensure_regime_columns=ensure_regime_columns,
        **kwargs,
    )


def _parse_args(argv: Optional[Iterable[str]] = None) -> argparse.Namespace:
    parser = argparse.ArgumentParser(
        description="Per-bar backtest vs live decision parity diff (#906 D7.4)",
    )
    parser.add_argument("strategy", nargs="?", help="Open strategy name")
    parser.add_argument("symbol", nargs="?", default="BTC/USDT")
    parser.add_argument("timeframe", nargs="?", default="1d")
    parser.add_argument("--since", default="2022-01-01", help="Start date (cached data)")
    parser.add_argument("--until", default="", help="Optional end date (inclusive)")
    parser.add_argument("--limit", type=int, default=0, help="Max bars after warmup")
    parser.add_argument("--registry", default="spot", choices=("spot", "futures"))
    parser.add_argument("--platform", default="binanceus")
    parser.add_argument("--params", default="", help="JSON strategy params override")
    parser.add_argument("--config", default="", help="Live config JSON (v13+)")
    parser.add_argument("--strategy-id", default="", help="Strategy id inside --config")
    parser.add_argument("--close", action="append", default=[], help="Close ref as name or name:json")
    parser.add_argument("--regime-enabled", action="store_true")
    parser.add_argument("--regime-period", type=int, default=14)
    parser.add_argument("--regime-adx-threshold", type=float, default=20.0)
    parser.add_argument("--no-fills", action="store_true", help="Skip backtester fill extraction")
    parser.add_argument("--output", default="", help="Write JSONL diff to this path")
    parser.add_argument("--summary-only", action="store_true")
    return parser.parse_args(list(argv) if argv is not None else None)


def _parse_close_refs(raw_list: list[str]) -> list[dict]:
    refs: list[dict] = []
    for item in raw_list:
        if ":" in item:
            name, params_json = item.split(":", 1)
            refs.append({"name": name.strip(), "params": json.loads(params_json)})
        else:
            refs.append({"name": item.strip(), "params": {}})
    return refs


def main(argv: Optional[Iterable[str]] = None) -> int:
    args = _parse_args(argv)

    if args.config:
        if not args.strategy_id:
            print("error: --strategy-id is required with --config", file=sys.stderr)
            return 2
        loaded = load_strategy_config(args.config, args.strategy_id)
        open_ref = loaded["open_strategy"]
        cfg = ParityConfig(
            strategy_name=open_ref["name"],
            symbol=args.symbol,
            timeframe=args.timeframe,
            params=dict(open_ref.get("params") or {}),
            registry="futures" if loaded.get("strategy_type") in ("perps", "futures", "manual") else "spot",
            platform=args.platform,
            open_strategy=open_ref["name"],
            close_strategies=loaded.get("close_strategies") or None,
            regime_enabled=args.regime_enabled,
            regime_period=args.regime_period,
            regime_adx_threshold=args.regime_adx_threshold,
        )
    else:
        if not args.strategy:
            print("error: strategy name required (or use --config + --strategy-id)", file=sys.stderr)
            return 2
        params = json.loads(args.params) if args.params else None
        reg = load_registry(args.registry)
        strat = reg.STRATEGY_REGISTRY.get(args.strategy)
        if not strat:
            print(f"error: unknown strategy {args.strategy!r}", file=sys.stderr)
            return 2
        cfg = ParityConfig(
            strategy_name=args.strategy,
            symbol=args.symbol,
            timeframe=args.timeframe,
            params=params or dict(strat.get("default_params") or {}),
            registry=args.registry,
            platform=args.platform,
            close_strategies=_parse_close_refs(args.close) or None,
            regime_enabled=args.regime_enabled,
            regime_period=args.regime_period,
            regime_adx_threshold=args.regime_adx_threshold,
        )

    from data_fetcher import load_cached_data

    df = load_cached_data(cfg.symbol, cfg.timeframe, start_date=args.since)
    if df.empty:
        print("error: no cached data — run data fetch first", file=sys.stderr)
        return 2
    if args.until:
        until = pd.Timestamp(args.until, tz="UTC")
        df = df[df.index <= until]
    if args.limit > 0:
        df = df.iloc[-(args.limit + cfg.min_warmup) :]

    try:
        result = run_from_dataframe(df, cfg, include_fills=not args.no_fills)
    except Exception as exc:
        print(f"error: {exc}", file=sys.stderr)
        return 2

    print(format_summary(result), file=sys.stderr)

    if args.output:
        with open(args.output, "w") as fh:
            for row in result.rows:
                fh.write(json.dumps(row, sort_keys=True) + "\n")
            for fill in result.fills:
                fh.write(json.dumps({"fill": fill}, sort_keys=True) + "\n")
    elif not args.summary_only:
        for row in result.rows:
            print(json.dumps(row, sort_keys=True))

    return 1 if result.mismatches else 0


if __name__ == "__main__":
    raise SystemExit(main())

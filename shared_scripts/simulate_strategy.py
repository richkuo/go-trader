#!/usr/bin/env python3
"""Replay strategy signals over dashboard candles for what-if preview (#811)."""

from __future__ import annotations

import argparse
import json
import os
import sys
from datetime import datetime, timezone
from typing import Any, Dict, List

import pandas as pd

ROOT = os.path.abspath(os.path.join(os.path.dirname(__file__), ".."))
sys.path.insert(0, ROOT)
sys.path.insert(0, os.path.join(ROOT, "shared_tools"))
sys.path.insert(0, os.path.join(ROOT, "backtest"))

from atr import ensure_atr_indicator  # noqa: E402
from regime import normalize_regime_gate_on_failure  # noqa: E402
from backtester import Backtester  # noqa: E402
from registry_loader import load_registry, registry_for_strategy_type  # noqa: E402
from run_backtest import _apply_htf_filter_to_df  # noqa: E402


def _fee_platform(platform: str, strategy_type: str) -> str:
    if strategy_type in ("perps", "manual") and platform == "hyperliquid":
        return "hyperliquid"
    if strategy_type == "perps" and platform == "okx":
        return "okx-perps"
    if platform in ("binanceus", "hyperliquid", "robinhood", "luno", "okx", "okx-perps"):
        return platform
    return "binanceus"


def _resolve_gate_on_failure(cfg: Dict[str, Any], regime_cfg: Dict[str, Any]) -> str:
    """#1278/#1300: resolve the entry-gate failure policy for the tuner path.

    Per-strategy ``regime_gate_on_failure`` wins over the global
    ``regime.gate_on_failure``, else the ``"open"`` default. Both surfaces are
    validated independently via the shared ``normalize_regime_gate_on_failure``
    SSoT so a garbage global value raises even when a valid per-strategy
    override would otherwise short-circuit past it (mirroring Go validateConfig
    rejecting unknown values on each surface).
    """
    per_raw = str(cfg.get("regime_gate_on_failure") or "").strip().lower()
    glob_gate = normalize_regime_gate_on_failure(regime_cfg.get("gate_on_failure"))
    return normalize_regime_gate_on_failure(per_raw) if per_raw else glob_gate


def _candles_to_df(candles: List[dict]) -> pd.DataFrame:
    if not candles:
        return pd.DataFrame()
    rows = []
    index = []
    for candle in candles:
        ts = int(candle.get("time") or 0)
        if ts <= 0:
            continue
        dt = datetime.fromtimestamp(ts, tz=timezone.utc)
        rows.append(
            {
                "open": float(candle.get("open") or 0),
                "high": float(candle.get("high") or 0),
                "low": float(candle.get("low") or 0),
                "close": float(candle.get("close") or 0),
                "volume": float(candle.get("volume") or 0),
            }
        )
        index.append(dt)
    if not rows:
        return pd.DataFrame()
    df = pd.DataFrame(rows, index=pd.DatetimeIndex(index, tz=timezone.utc))
    return df.sort_index()


def _trade_to_markers(trade: dict) -> List[dict]:
    markers: List[dict] = []
    entry_ts = _parse_ts(trade.get("entry_date"))
    exit_ts = _parse_ts(trade.get("exit_date"))
    side = str(trade.get("side") or "long").lower()
    entry_price = float(trade.get("entry_price") or 0)
    exit_price = float(trade.get("exit_price") or 0)
    pnl = float(trade.get("pnl") or 0)

    if entry_ts:
        if side == "short":
            markers.append(_marker(entry_ts, "sell", False, entry_price))
        else:
            markers.append(_marker(entry_ts, "buy", False, entry_price))
    if exit_ts:
        markers.append(_marker(exit_ts, "close", True, exit_price, pnl))
    return markers


def _parse_ts(raw: Any) -> int:
    if raw is None or raw == "" or raw == "None":
        return 0
    if isinstance(raw, (int, float)):
        return int(raw)
    text = str(raw).strip()
    if not text:
        return 0
    try:
        if text.isdigit():
            return int(text)
        dt = pd.Timestamp(text)
        if dt.tzinfo is None:
            dt = dt.tz_localize("UTC")
        else:
            dt = dt.tz_convert("UTC")
        return int(dt.timestamp())
    except Exception:
        return 0


def _marker(time: int, kind: str, is_close: bool, price: float, pnl: float = 0.0) -> dict:
    if kind == "buy":
        return {
            "time": time,
            "position": "belowBar",
            "color": "#059669",
            "shape": "arrowUp",
            "text": "BUY",
            "side": "buy",
            "is_close": False,
            "price": price,
        }
    if kind == "sell":
        return {
            "time": time,
            "position": "aboveBar",
            "color": "#dc2626",
            "shape": "arrowDown",
            "text": "SELL",
            "side": "sell",
            "is_close": False,
            "price": price,
        }
    return {
        "time": time,
        "position": "aboveBar",
        "color": "#2563eb",
        "shape": "circle",
        "text": "CLOSE",
        "side": "sell" if is_close else "buy",
        "is_close": True,
        "price": price,
        "realized_pnl": pnl,
    }


def _simulate_one(cfg: dict, candles: List[dict]) -> List[dict]:
    strategy_type = str(cfg.get("type") or "spot")
    if strategy_type == "options":
        raise ValueError("options strategies are not supported by the tuner preview yet")

    platform = str(cfg.get("platform") or "binanceus")
    symbol = str(cfg.get("symbol") or "")
    timeframe = str(cfg.get("timeframe") or "")
    open_ref = dict(cfg.get("open_strategy") or {})
    open_name = str(open_ref.get("name") or cfg.get("strategy") or "").strip()
    if not open_name:
        raise ValueError("missing open strategy name")

    reg = load_registry(registry_for_strategy_type(strategy_type))
    if open_name not in reg.STRATEGY_REGISTRY:
        raise ValueError(f"unknown strategy {open_name!r}")

    df = _candles_to_df(candles)
    if df.empty or len(df) < 3:
        return []

    params = dict(open_ref.get("params") or {})
    defaults = dict(reg.STRATEGY_REGISTRY[open_name].get("default_params") or {})
    merged_params = {**defaults, **params}

    df_signals = reg.apply_strategy(open_name, df, merged_params)
    # #842: a strategy has a single close_strategy ref; still accept the legacy
    # close_strategies array (length <=1 after the collapse). The Backtester's
    # close_strategies= list interface is fed the 0-or-1 element list.
    single_close = cfg.get("close_strategy")
    if isinstance(single_close, dict) and single_close.get("name"):
        close_refs = [dict(single_close)]
    else:
        legacy = cfg.get("close_strategies") or []
        # Match the live Go loader (#842): the array collapsed to a single
        # close_strategy, so reject a len>1 legacy array instead of previewing
        # it under the old max-fraction semantics the scheduler no longer runs.
        if len(legacy) > 1:
            raise ValueError(
                f"{len(legacy)} close_strategies supplied; the array model was "
                "collapsed to a single close_strategy (#842) — keep one "
                "profit-taking close and move risk backstops to strategy-level "
                "stop fields"
            )
        close_refs = [dict(r) for r in legacy]
    if close_refs:
        # #1277: Go stamps the RESOLVED atr_method (per-strategy > global >
        # simple) into each simulate payload so the preview's injected ATR
        # matches the live cycle's --atr-method. ensure_atr_indicator
        # validates the vocabulary (fails loud on an unknown value).
        df_signals = ensure_atr_indicator(
            df_signals, method=str(cfg.get("atr_method") or "simple")
        )

    if cfg.get("htf_filter"):
        df_signals = _apply_htf_filter_to_df(df_signals, symbol, timeframe)

    regime_cfg = dict(cfg.get("regime") or {})
    regime_enabled = bool(regime_cfg.get("enabled"))
    allowed = list(cfg.get("allowed_regimes") or [])
    # #1278: entry-gate failure policy — per-strategy field over the global
    # regime.gate_on_failure default, mirroring the live resolution order.
    gate_on_failure = _resolve_gate_on_failure(cfg, regime_cfg)

    bt = Backtester(
        initial_capital=float(cfg.get("initial_capital") or 1000),
        platform=_fee_platform(platform, strategy_type),
        open_strategy={"name": open_name, "params": merged_params},
        close_strategies=close_refs,
        regime_enabled=regime_enabled,
        regime_period=int(regime_cfg.get("period") or 14),
        regime_adx_threshold=float(regime_cfg.get("adx_threshold") or 20),
        allowed_regimes=allowed,
        regime_gate_on_failure=gate_on_failure,
        stop_loss_atr_mult=cfg.get("stop_loss_atr_mult"),
        stop_loss_pct=cfg.get("stop_loss_pct"),
        stop_loss_margin_pct=cfg.get("stop_loss_margin_pct"),
        trailing_stop_atr_mult=cfg.get("trailing_stop_atr_mult"),
        trailing_stop_pct=cfg.get("trailing_stop_pct"),
        stop_loss_atr_regime=cfg.get("stop_loss_atr_regime"),
        trailing_stop_atr_regime=cfg.get("trailing_stop_atr_regime"),
        strategy_type=strategy_type,
    )
    results = bt.run(
        df_signals,
        strategy_name=open_name,
        symbol=symbol,
        timeframe=timeframe,
        params=merged_params,
        save=False,
    )
    markers: List[dict] = []
    for trade in results.get("trades") or []:
        markers.extend(_trade_to_markers(trade))
    markers.sort(key=lambda m: (m.get("time") or 0, m.get("text") or ""))
    return markers


def _run_payload(payload: dict) -> dict:
    candles = list(payload.get("candles") or [])
    configs = list(payload.get("configs") or [])
    if not candles:
        return {"error": "no candles supplied", "markers": {}}
    if not configs:
        return {"error": "no configs supplied", "markers": {}}

    out: Dict[str, List[dict]] = {}
    for item in configs:
        label = str(item.get("label") or "default")
        cfg = dict(item.get("config") or item)
        try:
            out[label] = _simulate_one(cfg, candles)
        except Exception as exc:
            return {"error": f"{label}: {exc}", "markers": out}
    return {"markers": out}


def main() -> None:
    parser = argparse.ArgumentParser()
    parser.add_argument("--probe-only", action="store_true")
    args = parser.parse_args()

    if args.probe_only:
        print(json.dumps({"ok": True}))
        return

    raw = sys.stdin.read()
    if not raw.strip():
        print(json.dumps({"error": "empty stdin"}))
        sys.exit(1)
    try:
        payload = json.loads(raw)
    except json.JSONDecodeError as exc:
        print(json.dumps({"error": f"invalid json: {exc}"}))
        sys.exit(1)

    try:
        result = _run_payload(payload)
    except Exception as exc:
        print(json.dumps({"error": str(exc), "markers": {}}))
        sys.exit(1)

    if result.get("error"):
        print(json.dumps(result))
        sys.exit(1)
    print(json.dumps(result))


if __name__ == "__main__":
    main()

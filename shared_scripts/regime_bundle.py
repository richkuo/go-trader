#!/usr/bin/env python3
"""Compute raw regime metrics for the Go scheduler's cycle-local regime store."""

from __future__ import annotations

import argparse
import json
import math
import os
import sys

import pandas as pd

ROOT = os.path.abspath(os.path.join(os.path.dirname(__file__), ".."))
sys.path.insert(0, os.path.join(ROOT, "shared_tools"))

from fetch_candles import _fetch  # type: ignore
from regime import _atr_at_end, _composite_efficiency_metrics, compute_regime  # type: ignore


def _clean_float(value) -> float:
    try:
        value = float(value)
    except (TypeError, ValueError):
        return 0.0
    if not math.isfinite(value):
        return 0.0
    return value


def _rows_to_df(rows) -> pd.DataFrame:
    clean = []
    for row in rows or []:
        if len(row) < 5:
            continue
        clean.append(
            {
                "timestamp": int(row[0]),
                "open": _clean_float(row[1]),
                "high": _clean_float(row[2]),
                "low": _clean_float(row[3]),
                "close": _clean_float(row[4]),
                "volume": _clean_float(row[5]) if len(row) > 5 else 0.0,
            }
        )
    return pd.DataFrame(clean)


def _raw_metrics(df: pd.DataFrame, period: int) -> dict:
    if df is None or df.empty or len(df) <= period:
        return {"error": f"insufficient candles for period {period}: got {0 if df is None else len(df)}"}

    reg_df = compute_regime(df, period=period, adx_threshold=0.0)
    last = reg_df.iloc[-1]
    atr_val = _atr_at_end(df, period)
    close_val = _clean_float(df["close"].iloc[-1])
    metrics = {
        "adx": _clean_float(last.get("adx")),
        "plus_di": _clean_float(last.get("plus_di")),
        "minus_di": _clean_float(last.get("minus_di")),
        "atr_pct": round((atr_val / close_val * 100.0), 4) if close_val else 0.0,
    }
    if atr_val > 0 and len(df) >= period:
        window = df.iloc[-period:]
        eff = _composite_efficiency_metrics(window, atr_val, period)
        metrics.update(
            {
                "return_eff": round(_clean_float(eff.get("return_eff")), 4),
                "range_eff": round(_clean_float(eff.get("range_eff")), 4),
                "efficiency": round(_clean_float(eff.get("efficiency")), 4),
            }
        )
    return metrics


def main() -> None:
    parser = argparse.ArgumentParser()
    parser.add_argument("--platform", default="binanceus")
    parser.add_argument("--type", default="spot")
    parser.add_argument("--symbol", required=False, default="BTC/USDT")
    parser.add_argument("--timeframe", required=False, default="1h")
    parser.add_argument("--period", type=int, default=14)
    parser.add_argument("--limit", type=int, default=200)
    parser.add_argument("--mode", default="")
    parser.add_argument("--probe-only", action="store_true")
    args = parser.parse_args()

    if args.probe_only:
        print(json.dumps({"ok": True}))
        return
    if args.period < 2:
        print(json.dumps({"error": f"period must be >= 2, got {args.period}"}))
        sys.exit(1)

    try:
        rows, source = _fetch(args)
        df = _rows_to_df(rows)
        metrics = _raw_metrics(df, args.period)
        out = {
            "symbol": args.symbol,
            "timeframe": args.timeframe,
            "period": args.period,
            "source": source,
            "bar_time": int(df["timestamp"].iloc[-1]) // 1000 if df is not None and not df.empty else 0,
            "metrics": metrics,
        }
        if "error" in metrics:
            out["error"] = metrics["error"]
            out["metrics"] = {}
            print(json.dumps(out))
            sys.exit(1)
        print(json.dumps(out))
    except Exception as exc:
        print(json.dumps({"error": str(exc), "symbol": args.symbol, "timeframe": args.timeframe, "period": args.period, "metrics": {}}))
        sys.exit(1)


if __name__ == "__main__":
    main()

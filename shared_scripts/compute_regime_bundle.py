#!/usr/bin/env python3
"""Dedicated regime bundle subprocess for the Go scheduler (#879).

Fetches OHLCV once per (platform, type, symbol, timeframe, period) raw key per cycle
and returns raw ADX/efficiency metrics plus default 3-state / 7-state labels.
"""

from __future__ import annotations

import argparse
import json
import os
import sys

ROOT = os.path.abspath(os.path.join(os.path.dirname(__file__), ".."))
sys.path.insert(0, os.path.join(ROOT, "shared_tools"))
sys.path.insert(0, os.path.join(ROOT, "shared_scripts"))

from fetch_candles import _fetch  # noqa: E402
from regime import compute_regime_bundle, required_ohlcv_limit  # noqa: E402


def _df_from_candles(candles: list) -> "object":
    import pandas as pd

    if not candles:
        return pd.DataFrame()
    rows = []
    for c in candles:
        rows.append(
            [
                int(c["time"]) * 1000,
                float(c["open"]),
                float(c["high"]),
                float(c["low"]),
                float(c["close"]),
                float(c.get("volume") or 0),
            ]
        )
    return pd.DataFrame(rows, columns=["timestamp", "open", "high", "low", "close", "volume"])


def main() -> None:
    parser = argparse.ArgumentParser()
    parser.add_argument("--platform", required=True)
    parser.add_argument("--type", default="perps")
    parser.add_argument("--symbol", required=True)
    parser.add_argument("--timeframe", required=True)
    parser.add_argument("--period", type=int, default=14)
    parser.add_argument("--ohlcv-limit", type=int, default=0)
    parser.add_argument("--mode", default="")
    parser.add_argument("--probe-only", action="store_true")
    args = parser.parse_args()

    if args.probe_only:
        print(json.dumps({"ok": True}))
        return

    limit = args.ohlcv_limit if args.ohlcv_limit > 0 else required_ohlcv_limit(args.period)
    fetch_args = argparse.Namespace(
        platform=args.platform,
        type=args.type,
        symbol=args.symbol,
        timeframe=args.timeframe,
        limit=limit,
        from_time="",
        to_time="",
        mode=args.mode,
        probe_only=False,
    )
    try:
        rows, _source = _fetch(fetch_args)
    except Exception as exc:
        print(json.dumps({"ok": False, "error": str(exc)}))
        sys.exit(1)

    df = _df_from_candles(rows)
    bundle = compute_regime_bundle(df, args.period)
    if bundle is None:
        print(json.dumps({"ok": False, "error": "insufficient_ohlcv"}))
        sys.exit(1)

    print(json.dumps({"ok": True, **bundle}))


if __name__ == "__main__":
    main()

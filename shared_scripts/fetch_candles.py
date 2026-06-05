#!/usr/bin/env python3
"""Fetch OHLCV candles for the embedded scheduler dashboard."""

import argparse
import json
import math
import os
import sys
from datetime import datetime, timezone

ROOT = os.path.abspath(os.path.join(os.path.dirname(__file__), ".."))
sys.path.insert(0, os.path.join(ROOT, "shared_tools"))


def _parse_time(raw):
    if not raw:
        return None
    try:
        if raw.isdigit():
            return datetime.fromtimestamp(int(raw), tz=timezone.utc)
        return datetime.fromisoformat(raw.replace("Z", "+00:00")).astimezone(timezone.utc)
    except Exception:
        return None


def _clean_float(value):
    try:
        value = float(value)
    except (TypeError, ValueError):
        return 0.0
    if not math.isfinite(value):
        return 0.0
    return value


def _row_to_candle(row):
    ts = int(row[0])
    return {
        "time": ts // 1000,
        "open": _clean_float(row[1]),
        "high": _clean_float(row[2]),
        "low": _clean_float(row[3]),
        "close": _clean_float(row[4]),
        "volume": _clean_float(row[5]) if len(row) > 5 else 0.0,
    }


def _df_to_rows(df):
    if df is None or df.empty:
        return []
    rows = []
    for _, row in df.reset_index().iterrows():
        ts = int(row["timestamp"])
        rows.append([ts, row["open"], row["high"], row["low"], row["close"], row.get("volume", 0)])
    return rows


def _load_adapter(platform, mode):
    if platform == "hyperliquid":
        sys.path.insert(0, os.path.join(ROOT, "platforms", "hyperliquid"))
        from adapter import HyperliquidExchangeAdapter

        return HyperliquidExchangeAdapter()
    if platform == "okx":
        sys.path.insert(0, os.path.join(ROOT, "platforms", "okx"))
        from adapter import OKXExchangeAdapter

        return OKXExchangeAdapter()
    if platform == "topstep":
        sys.path.insert(0, os.path.join(ROOT, "platforms", "topstep"))
        from adapter import TopStepExchangeAdapter

        return TopStepExchangeAdapter(mode=mode or "paper")
    if platform == "robinhood":
        sys.path.insert(0, os.path.join(ROOT, "platforms", "robinhood"))
        from adapter import RobinhoodExchangeAdapter

        return RobinhoodExchangeAdapter(mode=mode or "paper")
    return None


def _fetch(args):
    adapter = _load_adapter(args.platform, args.mode)
    if adapter is not None:
        if args.platform == "okx" and args.type == "perps":
            rows = adapter.get_perp_ohlcv(args.symbol, interval=args.timeframe, limit=args.limit)
        else:
            rows = adapter.get_ohlcv(args.symbol, interval=args.timeframe, limit=args.limit)
        return rows or [], f"{args.platform}:adapter"

    from data_fetcher import fetch_ohlcv

    symbol = args.symbol
    if "/" not in symbol and args.type in ("perps", "manual"):
        symbol = f"{symbol}/USDT"
    exchange_id = args.platform if args.platform else "binanceus"
    if exchange_id in ("manual", "hyperliquid"):
        exchange_id = "binanceus"
    if args.type == "options" and adapter is None:
        exchange_id = "binanceus"
        if "/" not in symbol:
            symbol = f"{symbol}/USDT"
    df = fetch_ohlcv(
        symbol=symbol,
        timeframe=args.timeframe,
        limit=args.limit,
        exchange_id=exchange_id,
        store=False,
    )
    return _df_to_rows(df), f"{exchange_id}:ccxt"


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument("--platform", default="binanceus")
    parser.add_argument("--type", default="spot")
    parser.add_argument("--symbol", required=True)
    parser.add_argument("--timeframe", required=True)
    parser.add_argument("--limit", type=int, default=300)
    parser.add_argument("--from", dest="from_time", default="")
    parser.add_argument("--to", dest="to_time", default="")
    parser.add_argument("--mode", default="")
    parser.add_argument("--probe-only", action="store_true")
    args = parser.parse_args()

    if args.probe_only:
        print(json.dumps({"ok": True}))
        return

    try:
        rows, source = _fetch(args)
        from_time = _parse_time(args.from_time)
        to_time = _parse_time(args.to_time)
        candles = []
        for row in rows:
            candle = _row_to_candle(row)
            t = datetime.fromtimestamp(candle["time"], tz=timezone.utc)
            if from_time and t < from_time:
                continue
            if to_time and t > to_time:
                continue
            candles.append(candle)
        candles.sort(key=lambda c: c["time"])
        print(json.dumps({"source": source, "candles": candles}))
    except Exception as exc:
        print(json.dumps({"error": str(exc), "source": "error", "candles": []}))
        sys.exit(1)


if __name__ == "__main__":
    main()

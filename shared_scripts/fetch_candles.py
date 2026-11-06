#!/usr/bin/env python3

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


_ROBINHOOD_CHART_CLASSES = {"spot": "crypto", "options": "stock"}


def _robinhood_chart_class(chart_type, symbol):
    asset_class = _ROBINHOOD_CHART_CLASSES.get(chart_type)
    if asset_class is None:
        raise ValueError(
            f"robinhood charts serve type spot (crypto) or options (stock underlying), not {chart_type!r}")
    sys.path.insert(0, os.path.join(ROOT, "platforms", "robinhood"))
    from adapter import robinhood_yahoo_ticker

    robinhood_yahoo_ticker(asset_class, symbol)
    return asset_class


def _load_adapter(platform, mode, chart_type="spot", symbol=""):
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
        asset_class = _robinhood_chart_class(chart_type, symbol)
        from adapter import RobinhoodExchangeAdapter

        return RobinhoodExchangeAdapter(mode=mode or "paper", asset_class=asset_class)
    return None


def _fetch(args):
    adapter = _load_adapter(args.platform, args.mode, args.type, args.symbol)
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
    df = fetch_ohlcv(
        symbol=symbol,
        timeframe=args.timeframe,
        limit=args.limit,
        exchange_id=exchange_id,
        store=False,
    )
    return _df_to_rows(df), f"{exchange_id}:ccxt"


def _parse_bound(raw, name):
    text = (raw or "").strip()
    if not text:
        raise ValueError(f"--{name} is empty")
    try:
        if text.isdigit():
            parsed = datetime.fromtimestamp(int(text), tz=timezone.utc)
        else:
            parsed = datetime.fromisoformat(text.replace("Z", "+00:00"))
    except (ValueError, OverflowError, OSError) as exc:
        raise ValueError(f"--{name} {raw!r} is not a Unix time or ISO-8601 instant: {exc}") from exc
    if parsed.tzinfo is None:
        raise ValueError(f"--{name} {raw!r} has no UTC offset")
    return parsed.astimezone(timezone.utc)


def _range_bounds_ms(args):
    start = _parse_bound(args.from_time, "from")
    end = _parse_bound(args.to_time, "to") if (args.to_time or "").strip() else datetime.now(timezone.utc)
    if end < start:
        raise ValueError(f"--to {end.isoformat()} is before --from {start.isoformat()}")
    start_ms = int(round(start.timestamp() * 1000))
    end_ms = int(round(end.timestamp() * 1000))
    return start_ms, end_ms


def _range_adapter(platform):
    if platform == "hyperliquid":
        sys.path.insert(0, os.path.join(ROOT, "platforms", "hyperliquid"))
        from adapter import HyperliquidExchangeAdapter

        return HyperliquidExchangeAdapter(public_only=True)
    if platform == "okx":
        sys.path.insert(0, os.path.join(ROOT, "platforms", "okx"))
        from adapter import OKXExchangeAdapter

        return OKXExchangeAdapter(public_only=True)
    raise ValueError(f"no range adapter for platform {platform!r}")


def _fetch_range(args, start_ms, end_ms):
    from ohlcv_range import timeframe_ms

    timeframe_ms(args.timeframe)
    if args.platform == "hyperliquid":
        res = _range_adapter("hyperliquid").get_ohlcv_range(args.symbol, args.timeframe, start_ms, end_ms)
        meta = {"supported": True, "requests": res["pages"], "truncated": res["truncated"],
                "stalled": res["stalled"], "instrument": args.symbol}
        return res["rows"], "hyperliquid:adapter:range", meta
    if args.platform == "okx":
        inst_type = "swap" if args.type == "perps" else "spot"
        res = _range_adapter("okx").get_ohlcv_range(args.symbol, args.timeframe, start_ms, end_ms, inst_type=inst_type)
        meta = {"supported": True, "requests": res["requests"], "truncated": res["truncated"],
                "stalled": res["stalled"], "instrument": res["instrument"]}
        return res["rows"], "okx:adapter:range", meta
    if args.platform in ("topstep", "robinhood"):
        adapter = _load_adapter(args.platform, args.mode, args.type, args.symbol)
        rows = adapter.get_ohlcv(args.symbol, interval=args.timeframe, limit=args.limit)
        meta = {"supported": False, "requests": 1, "truncated": False, "stalled": False, "instrument": args.symbol,
                "reason": f"{args.platform} serves only a latest window; older bars in the range may be missing"}
        return rows or [], f"{args.platform}:adapter:latest", meta
    exchange_id = args.platform if args.platform else "binanceus"
    if exchange_id == "manual":
        raise ValueError("platform 'manual' has no historical candle source")
    symbol = args.symbol
    if "/" not in symbol and args.type in ("perps", "manual"):
        symbol = f"{symbol}/USDT"
    from data_fetcher import fetch_ohlcv_range

    res = fetch_ohlcv_range(symbol, args.timeframe, start_ms, end_ms, exchange_id=exchange_id)
    meta = {"supported": True, "requests": res["requests"], "truncated": res["truncated"],
            "stalled": res["stalled"], "instrument": symbol}
    return res["rows"], f"{exchange_id}:ccxt:range", meta


def _range_row_to_candle(row):
    try:
        ts = int(row[0])
        values = [float(v) for v in row[1:5]]
        volume = float(row[5]) if len(row) > 5 and row[5] is not None else 0.0
    except (TypeError, ValueError, IndexError):
        return None
    if not all(math.isfinite(v) and v > 0 for v in values) or not math.isfinite(volume) or volume < 0:
        return None
    o, h, l, c = values
    if h < max(o, c, l) or l > min(o, c, h):
        return None
    return {"time": ts // 1000, "open": o, "high": h, "low": l, "close": c, "volume": volume}


def _range_output(args):
    start_ms, end_ms = _range_bounds_ms(args)
    rows, source, meta = _fetch_range(args, start_ms, end_ms)
    candles = []
    invalid = 0
    for row in rows:
        candle = _range_row_to_candle(row)
        if candle is None:
            invalid += 1
            continue
        t_ms = candle["time"] * 1000
        if t_ms < start_ms or t_ms > end_ms:
            continue
        candles.append(candle)
    candles.sort(key=lambda c: c["time"])
    meta.update({
        "from_ms": start_ms,
        "to_ms": end_ms,
        "timestamp": "open" if meta["supported"] else "unverified",
        "invalid_rows": invalid,
    })
    return {"source": source, "candles": candles, "range": meta}


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
        if args.platform == "robinhood":
            try:
                _robinhood_chart_class(args.type, args.symbol)
            except ValueError as exc:
                print(json.dumps({"error": str(exc), "source": "error", "candles": []}))
                sys.exit(1)
        print(json.dumps({"ok": True}))
        return

    if (args.from_time or "").strip():
        try:
            print(json.dumps(_range_output(args)))
        except Exception as exc:
            print(json.dumps({"error": str(exc), "source": "error", "candles": []}))
            sys.exit(1)
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

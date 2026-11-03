#!/usr/bin/env python3

from __future__ import annotations

import argparse
import importlib.util
import json
import os
import sys
from datetime import datetime, timezone

_REPO_ROOT = os.path.abspath(os.path.join(os.path.dirname(__file__), ".."))
sys.path.insert(0, os.path.join(_REPO_ROOT, "shared_tools"))

import egress_policy
from market_payload import market_frame_rows
from regime import (
    CLASSIFIER_ADX,
    CLASSIFIER_COMPOSITE,
    _DEFAULT_COMPOSITE_THRESHOLDS,
    compute_multi_regime,
    latest_regime,
    latest_regime_composite,
    parse_regime_windows_spec_json,
)


ROBINHOOD_ASSET_CLASSES = ("crypto", "stock")


def _emit_error(args, message: str) -> None:
    doc = {
        "platform": getattr(args, "platform", ""),
        "symbol": getattr(args, "symbol", ""),
        "timeframe": getattr(args, "timeframe", ""),
        "regime": None,
        "error": message,
        "timestamp": datetime.now(timezone.utc).isoformat(),
    }
    if getattr(args, "asset_class", ""):
        doc["asset_class"] = args.asset_class
    print(json.dumps(doc))
    sys.exit(1)


def _asset_class_error(args) -> str:
    platform = args.platform.strip().lower()
    if platform == "robinhood":
        if args.asset_class not in ROBINHOOD_ASSET_CLASSES:
            return (f"--asset-class must be one of {', '.join(ROBINHOOD_ASSET_CLASSES)} for platform robinhood, "
                    f"got {args.asset_class!r}")
        return ""
    if args.asset_class:
        return f"--asset-class is only for platform robinhood, not {args.platform!r}"
    return ""


def _robinhood_symbol_error(args) -> str:
    sys.path.insert(0, os.path.join(_REPO_ROOT, "platforms", "robinhood"))
    from adapter import UnsupportedRobinhoodSymbol, robinhood_yahoo_ticker

    try:
        robinhood_yahoo_ticker(args.asset_class, args.symbol)
    except UnsupportedRobinhoodSymbol as e:
        return str(e)
    return ""


def _load_adapter(platform: str, asset_class: str = ""):
    adapter_path = os.path.join(_REPO_ROOT, "platforms", platform, "adapter.py")
    if not os.path.exists(adapter_path):
        raise ImportError(f"No adapter found for platform '{platform}' at {adapter_path}")
    platform_dir = os.path.dirname(adapter_path)
    if platform_dir not in sys.path:
        sys.path.insert(0, platform_dir)
    spec = importlib.util.spec_from_file_location(f"{platform}_adapter", adapter_path)
    module = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(module)
    kwargs = {"asset_class": asset_class} if platform == "robinhood" else {}
    for name in dir(module):
        if name.endswith("ExchangeAdapter"):
            return getattr(module, name)(**kwargs)
    raise AttributeError(f"No ExchangeAdapter class found in {adapter_path}")


def _make_dataframe(candles):
    import pandas as pd

    df = pd.DataFrame(candles, columns=["timestamp", "open", "high", "low", "close", "volume"])
    df["datetime"] = pd.to_datetime(df["timestamp"], unit="ms", utc=True)
    df = df.set_index("datetime")
    df.sort_index(inplace=True)
    return df


def _fetch_dataframe(args):
    platform = args.platform.strip().lower()
    if platform == "binanceus":
        from data_fetcher import fetch_ohlcv

        return fetch_ohlcv(symbol=args.symbol, timeframe=args.timeframe,
                           limit=args.ohlcv_limit, store=False)

    if platform == "robinhood":
        rows = _load_adapter(platform, args.asset_class).get_ohlcv(
            args.symbol, interval=args.timeframe, limit=args.ohlcv_limit)
        return _make_dataframe(rows) if rows else None

    rows = None
    adapter_err = None
    try:
        adapter = _load_adapter(platform)
        ohlcv_fn = getattr(adapter, "get_ohlcv", None)
        if ohlcv_fn is not None:
            rows = ohlcv_fn(args.symbol, interval=args.timeframe, limit=args.ohlcv_limit)
        elif not args.allow_spot_fallback:
            raise AttributeError(f"adapter for '{platform}' has no get_ohlcv")
    except Exception as e:
        if not args.allow_spot_fallback:
            raise
        adapter_err = e
        rows = None

    if not rows and args.allow_spot_fallback:
        if adapter_err is not None:
            print(f"adapter.get_ohlcv failed: {adapter_err}", file=sys.stderr)
        import ccxt

        exchange = egress_policy.pin_ccxt(ccxt.binanceus({"enableRateLimit": True}))
        rows = exchange.fetch_ohlcv(f"{args.symbol}/USDT", args.timeframe, limit=args.ohlcv_limit)

    if not rows:
        return None
    return _make_dataframe(rows)


def _market_dataframe(args):
    payload = json.loads(sys.stdin.read())
    if not isinstance(payload, dict):
        raise ValueError("market stdin payload must be a JSON object")
    market = payload.get("market")
    rows = market_frame_rows(market, args.symbol, args.timeframe, limit=args.ohlcv_limit)
    return _make_dataframe(rows)


def _window_adx_threshold(spec: dict) -> float:
    if spec.get("classifier") == CLASSIFIER_COMPOSITE:
        th = spec.get("thresholds") or {}
        return float(th.get("adx") or _DEFAULT_COMPOSITE_THRESHOLDS["adx"])
    return float(spec.get("adx_threshold") or 20.0)


def compute_regime_bundle(df, windows_spec: dict) -> dict:
    snapshots = compute_multi_regime(df, windows_spec)
    views: dict[str, dict[str, str]] = {}
    for name in sorted(windows_spec.keys()):
        spec = windows_spec[name]
        period = int(spec["period"])
        snap = snapshots.get(name) or {}
        adx3 = latest_regime(df, period=period, adx_threshold=_window_adx_threshold(spec))
        composite7 = latest_regime_composite(df, period, spec.get("thresholds"))
        views[name] = {"adx3": adx3, "composite7": composite7}
    return {"regime": snapshots, "views": views}


def main() -> None:
    parser = argparse.ArgumentParser(description="Compute one regime bundle (#879)")
    parser.add_argument("--platform", required=True,
                        help="data source: binanceus | hyperliquid | okx | topstep | robinhood | <options platform>")
    parser.add_argument("--symbol", required=True)
    parser.add_argument("--timeframe", required=True)
    parser.add_argument("--regime-windows-spec-json", required=True,
                        help="resolved window specs from Go regimeWindowsSpecJSON")
    parser.add_argument("--ohlcv-limit", type=int, default=200)
    parser.add_argument("--min-bars", type=int, default=30,
                        help="accepted for compatibility; each classifier reports its own readiness")
    parser.add_argument("--allow-spot-fallback", action="store_true", default=False,
                        help="options platforms: fall back to BinanceUS <symbol>/USDT when the adapter has no OHLCV")
    parser.add_argument("--market-stdin", action="store_true", default=False,
                        help="#1524: read a sealed market payload from stdin instead of fetching candles")
    parser.add_argument("--asset-class", default="",
                        help="#1909: robinhood only and required there: crypto (spot coin) or stock (options underlying)")
    parser.add_argument("--probe-only", action="store_true", default=False)
    args = parser.parse_args()
    class_error = _asset_class_error(args)
    if class_error:
        _emit_error(args, class_error)
    if args.probe_only:
        sys.exit(0)
    if args.platform.strip().lower() == "robinhood":
        symbol_error = _robinhood_symbol_error(args)
        if symbol_error:
            _emit_error(args, symbol_error)

    try:
        windows_spec = parse_regime_windows_spec_json(args.regime_windows_spec_json)
        if not windows_spec:
            _emit_error(args, "empty --regime-windows-spec-json")

        if args.market_stdin:
            df = _market_dataframe(args)
        else:
            df = _fetch_dataframe(args)
        if df is None or len(df) == 0:
            got = 0 if df is None else len(df)
            _emit_error(args, f"Insufficient data: {got} candles (need 1)")

        bundle = compute_regime_bundle(df, windows_spec)

        bar_time = ""
        try:
            idx_last = df.index[-1]
            bar_time = idx_last.isoformat() if hasattr(idx_last, "isoformat") else str(idx_last)
        except Exception:
            bar_time = ""

        doc = {
            "platform": args.platform,
            "symbol": args.symbol,
            "timeframe": args.timeframe,
            "bar_time": bar_time,
            "regime": bundle["regime"],
            "views": bundle["views"],
            "timestamp": datetime.now(timezone.utc).isoformat(),
        }
        if args.asset_class:
            doc["asset_class"] = args.asset_class
        print(json.dumps(doc))
    except SystemExit:
        raise
    except Exception as e:
        import traceback

        traceback.print_exc(file=sys.stderr)
        _emit_error(args, str(e))


if __name__ == "__main__":
    main()

#!/usr/bin/env python3
"""Standalone market-regime subprocess for the Go-side global regime store (#879).

The Go scheduler computes regime ONCE per distinct (platform, symbol, interval) per
cycle by invoking this script, instead of every per-strategy check recomputing it
inline. Because regime config is global (one classifier/period/thresholds set per
window, shared by all strategies on an asset), one bundle per asset serves every
strategy on it; each strategy then projects its own gate/atr/directional window
selector out of the shared multi-window payload.

This script is READ-ONLY (no orders, no state writes). It mirrors a check script's
OHLCV fetch (same limit ⇒ shares the #839 HL OHLCV /tmp cache) and reuses
`prepare_check_regime` so labels are byte-identical to the inline path.

Usage:
    python3 check_regime.py <symbol> <interval> --platform <p> \
        [--regime-windows-spec-json <json>] [--ohlcv-limit N] \
        [--regime-atr-window <key>] [--period N] [--adx-threshold X] \
        [--min-bars N] [--probe-only]

Output (stdout JSON, always — even on error):
    {"ok": true,  "regime": <multi-map-or-label>, "live_regime": "...", "bar_time": <ms>}
    {"ok": false, "error": "..."}            # exit 1
    {"ok": true,  "probe": true}             # --probe-only, exit 0, no network
"""

import argparse
import importlib.util
import json
import os
import sys
from datetime import datetime, timezone

_THIS_DIR = os.path.dirname(os.path.abspath(__file__))
_REPO_ROOT = os.path.dirname(_THIS_DIR)
sys.path.insert(0, _REPO_ROOT)
sys.path.insert(0, os.path.join(_REPO_ROOT, "shared_tools"))

from regime import prepare_check_regime, parse_regime_windows_spec_json  # noqa: E402

REGIME_MIN_BARS = 30


def _emit(obj, code=0):
    print(json.dumps(obj))
    sys.exit(code)


def _load_adapter(platform: str):
    """Load platforms/<platform>/adapter.py and return its *ExchangeAdapter().

    Returns None when the platform has no adapter (e.g. binanceus spot), so the
    caller falls back to data_fetcher.
    """
    adapter_path = os.path.join(_REPO_ROOT, "platforms", platform, "adapter.py")
    if not os.path.exists(adapter_path):
        return None
    platform_dir = os.path.dirname(adapter_path)
    if platform_dir not in sys.path:
        sys.path.insert(0, platform_dir)
    spec = importlib.util.spec_from_file_location(f"{platform}_adapter", adapter_path)
    module = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(module)
    for name in dir(module):
        if name.endswith("ExchangeAdapter"):
            return getattr(module, name)()
    raise AttributeError(f"No ExchangeAdapter class found in {adapter_path}")


def _rows_to_df(rows):
    import pandas as pd

    if not rows:
        return None
    return pd.DataFrame(
        rows, columns=["timestamp", "open", "high", "low", "close", "volume"]
    )


def _fetch_df(platform, symbol, interval, limit):
    """Fetch OHLCV as a DataFrame. Mirrors the existing check fetch paths so labels
    match: adapter platforms (HL/OKX/TopStep/Robinhood) use get_ohlcv positionally
    (same call/limit ⇒ shares the #839 HL cache); spot symbols (BTC/USDT) use
    data_fetcher; bare options underlyings (e.g. BTC on deribit) fall back to
    BinanceUS <sym>/USDT, exactly like check_options._fetch_ohlcv_df. None on failure."""
    adapter = _load_adapter(platform)
    if adapter is not None:
        ohlcv_fn = getattr(adapter, "get_ohlcv", None)
        if ohlcv_fn is not None:
            try:
                rows = ohlcv_fn(symbol, interval, limit)
            except Exception as e:
                print(f"adapter.get_ohlcv failed: {e}", file=sys.stderr)
                rows = None
            df = _rows_to_df(rows)
            if df is not None:
                return df

    # No adapter OHLCV. Spot pairs (with "/") go through data_fetcher; bare
    # underlyings (options on deribit/ibkr) use BinanceUS <sym>/USDT.
    if "/" in symbol:
        from data_fetcher import fetch_ohlcv

        return fetch_ohlcv(symbol=symbol, timeframe=interval, limit=limit, store=False)
    try:
        import ccxt

        exchange = ccxt.binanceus({"enableRateLimit": True})
        rows = exchange.fetch_ohlcv(f"{symbol}/USDT", interval, limit=limit)
        return _rows_to_df(rows)
    except Exception as e:
        print(f"BinanceUS OHLCV fallback failed: {e}", file=sys.stderr)
        return None


def _bar_time(df):
    try:
        return int(df["timestamp"].iloc[-1])
    except Exception:
        return None


def main():
    parser = argparse.ArgumentParser(description="Standalone regime computation (#879)")
    parser.add_argument("symbol")
    parser.add_argument("interval")
    parser.add_argument("--platform", required=True)
    parser.add_argument("--regime-windows-spec-json", default="")
    parser.add_argument("--regime-atr-window", default="")
    parser.add_argument("--ohlcv-limit", type=int, default=200)
    parser.add_argument("--period", type=int, default=14)
    parser.add_argument("--adx-threshold", type=float, default=20.0)
    parser.add_argument("--min-bars", type=int, default=REGIME_MIN_BARS)
    parser.add_argument("--probe-only", action="store_true", default=False)
    args = parser.parse_args()

    # Probe validates argv + imports without touching the network (read-only FS safe).
    if args.probe_only:
        _emit({"ok": True, "probe": True})

    try:
        windows_spec = parse_regime_windows_spec_json(args.regime_windows_spec_json or None)
    except Exception as e:
        _emit({"ok": False, "error": f"bad regime windows spec: {e}"}, code=1)

    try:
        df = _fetch_df(args.platform, args.symbol, args.interval, args.ohlcv_limit)
    except Exception as e:
        _emit({"ok": False, "error": f"ohlcv fetch failed: {e}"}, code=1)

    if df is None or len(df) < args.min_bars:
        n = 0 if df is None else len(df)
        _emit({"ok": False, "error": f"insufficient data: {n} bars"}, code=1)

    try:
        stdout_regime, live_regime, _strategy_regime = prepare_check_regime(
            df,
            regime_enabled=True,
            period=args.period,
            adx_threshold=args.adx_threshold,
            windows_spec=windows_spec,
            atr_window=args.regime_atr_window,
        )
    except Exception as e:
        _emit({"ok": False, "error": f"regime computation failed: {e}"}, code=1)

    _emit(
        {
            "ok": True,
            "regime": stdout_regime,
            "live_regime": live_regime,
            "bar_time": _bar_time(df),
            "computed_at": datetime.now(timezone.utc).isoformat(),
        }
    )


if __name__ == "__main__":
    try:
        main()
    except SystemExit:
        raise
    except Exception as e:  # noqa: BLE001 — emit JSON even on unexpected crash
        print(json.dumps({"ok": False, "error": str(e)}))
        sys.exit(1)

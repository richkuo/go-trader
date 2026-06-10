#!/usr/bin/env python3
"""Dedicated regime-bundle subprocess for the Go scheduler (#879).

Computes the market regime once per distinct (platform, symbol, timeframe,
windows-spec) signature per scheduler cycle. The Go scheduler stores the
emitted bundle in its per-cycle global regime store and injects it into each
check script via --regime-payload-json, so peer strategies sharing a signature
never recompute regime math inline.

Usage:
    check_regime.py --platform hyperliquid --symbol BTC --timeframe 1h \
        --regime-windows-spec-json '{"default":{"classifier":"adx","period":14,"adx_threshold":20}}' \
        --ohlcv-limit 200

Output (stdout, JSON):
    {
      "platform": "...", "symbol": "...", "timeframe": "...",
      "bar_time": "<ISO timestamp of the last bar in the fetched frame>",
      "regime": {"<window>": {"regime","score","classifier","metrics"}, ...},
      "views":  {"<window>": {"adx3": "...", "composite7": "..."}, ...},
      "timestamp": "<now>"
    }

The "regime" map is byte-compatible with the multi-window payload check
scripts emit from prepare_check_regime, so RegimePayload on the Go side and
regime_from_injected_payload on the Python side both consume it unchanged.
The "views" map adds both classifier vocabularies per window for the
portfolio/dashboard regime surface: "adx3" always runs the real ADX
classifier at the window's FULL period (exact parity with a standalone ADX
window even when period > COMPOSITE_ADX_PERIOD_CAP), never a prefix-collapse
of the composite label.

Errors follow the subprocess contract: JSON with "error" on stdout, exit 1.
"""

from __future__ import annotations

import argparse
import importlib.util
import json
import os
import sys
from datetime import datetime, timezone

_REPO_ROOT = os.path.abspath(os.path.join(os.path.dirname(__file__), ".."))
sys.path.insert(0, os.path.join(_REPO_ROOT, "shared_tools"))

from regime import (  # noqa: E402
    CLASSIFIER_ADX,
    CLASSIFIER_COMPOSITE,
    _DEFAULT_COMPOSITE_THRESHOLDS,
    compute_multi_regime,
    latest_regime,
    latest_regime_composite,
    parse_regime_windows_spec_json,
)


def _emit_error(args, message: str) -> None:
    print(json.dumps({
        "platform": getattr(args, "platform", ""),
        "symbol": getattr(args, "symbol", ""),
        "timeframe": getattr(args, "timeframe", ""),
        "regime": None,
        "error": message,
        "timestamp": datetime.now(timezone.utc).isoformat(),
    }))
    sys.exit(1)


def _load_adapter(platform: str):
    """Load ExchangeAdapter from platforms/<platform>/adapter.py (one per file).

    Mirrors check_options.py: insert the platform dir into sys.path before
    exec (the HL adapter needs its dir first to win the SDK name clash).
    """
    adapter_path = os.path.join(_REPO_ROOT, "platforms", platform, "adapter.py")
    if not os.path.exists(adapter_path):
        raise ImportError(f"No adapter found for platform '{platform}' at {adapter_path}")
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


def _make_dataframe(candles):
    """Raw OHLCV rows -> DataFrame; mirrors the per-platform check scripts."""
    import pandas as pd

    df = pd.DataFrame(candles, columns=["timestamp", "open", "high", "low", "close", "volume"])
    df["datetime"] = pd.to_datetime(df["timestamp"], unit="ms", utc=True)
    df = df.set_index("datetime")
    df.sort_index(inplace=True)
    return df


def _fetch_dataframe(args):
    """Fetch OHLCV from the same source the strategy's check script uses.

    binanceus -> shared_tools data_fetcher (default-spot check_strategy.py
    path); every other platform -> its ExchangeAdapter.get_ohlcv. When
    --allow-spot-fallback is set (options strategies), an adapter without
    get_ohlcv or a failed adapter fetch falls back to BinanceUS ccxt with
    "<symbol>/USDT", mirroring check_options._fetch_ohlcv_df. Without the
    flag there is NO cross-venue fallback: wrong-feed data is worse than an
    empty bundle (the consumers fail open on empty).
    """
    platform = args.platform.strip().lower()
    if platform == "binanceus":
        from data_fetcher import fetch_ohlcv

        return fetch_ohlcv(symbol=args.symbol, timeframe=args.timeframe,
                           limit=args.ohlcv_limit, store=False)

    rows = None
    adapter_err = None
    try:
        adapter = _load_adapter(platform)
        ohlcv_fn = getattr(adapter, "get_ohlcv", None)
        if ohlcv_fn is not None:
            rows = ohlcv_fn(args.symbol, interval=args.timeframe, limit=args.ohlcv_limit)
        elif not args.allow_spot_fallback:
            raise AttributeError(f"adapter for '{platform}' has no get_ohlcv")
    except Exception as e:  # noqa: BLE001 - converted to bundle error or fallback
        if not args.allow_spot_fallback:
            raise
        adapter_err = e
        rows = None

    if not rows and args.allow_spot_fallback:
        if adapter_err is not None:
            print(f"adapter.get_ohlcv failed: {adapter_err}", file=sys.stderr)
        import ccxt

        exchange = ccxt.binanceus({"enableRateLimit": True})
        rows = exchange.fetch_ohlcv(f"{args.symbol}/USDT", args.timeframe, limit=args.ohlcv_limit)

    if not rows:
        return None
    return _make_dataframe(rows)


def _window_adx_threshold(spec: dict) -> float:
    """ADX threshold for the 3-state view of one window spec."""
    if spec.get("classifier") == CLASSIFIER_COMPOSITE:
        th = spec.get("thresholds") or {}
        return float(th.get("adx") or _DEFAULT_COMPOSITE_THRESHOLDS["adx"])
    return float(spec.get("adx_threshold") or 20.0)


def compute_regime_bundle(df, windows_spec: dict) -> dict:
    """Compute the per-window snapshots + both-classifier views for one frame.

    Pure (no I/O) so tests can assert parity against prepare_check_regime.
    Snapshots come from the SAME compute_multi_regime call the check scripts
    used pre-#879, so a consumer's label is unchanged by the migration. Views
    run the alternate classifier per window: adx3 at the window's full period
    (exact ADX parity past COMPOSITE_ADX_PERIOD_CAP), composite7 at the
    window's period with its composite thresholds (defaults when the window
    is ADX-classified).
    """
    snapshots = compute_multi_regime(df, windows_spec)
    views: dict[str, dict[str, str]] = {}
    for name in sorted(windows_spec.keys()):
        spec = windows_spec[name]
        period = int(spec["period"])
        snap = snapshots.get(name) or {}
        if spec.get("classifier") == CLASSIFIER_COMPOSITE:
            composite7 = str(snap.get("regime") or "")
            adx3 = str(latest_regime(df, period=period,
                                     adx_threshold=_window_adx_threshold(spec)).get("regime") or "")
        else:
            adx3 = str(snap.get("regime") or "")
            composite7 = str(latest_regime_composite(
                df, period, spec.get("thresholds")).get("regime") or "")
        views[name] = {"adx3": adx3, "composite7": composite7}
    return {"regime": snapshots, "views": views}


def main() -> None:
    # #645-style startup compatibility probe — exit 0 before any work.
    if "--probe-only" in sys.argv:
        sys.exit(0)

    parser = argparse.ArgumentParser(description="Compute one regime bundle (#879)")
    parser.add_argument("--platform", required=True,
                        help="data source: binanceus | hyperliquid | okx | topstep | robinhood | <options platform>")
    parser.add_argument("--symbol", required=True)
    parser.add_argument("--timeframe", required=True)
    parser.add_argument("--regime-windows-spec-json", required=True,
                        help="resolved window specs from Go regimeWindowsSpecJSON")
    parser.add_argument("--ohlcv-limit", type=int, default=200)
    parser.add_argument("--min-bars", type=int, default=30,
                        help="insufficient-data floor; mirrors the check scripts' candle guard")
    parser.add_argument("--allow-spot-fallback", action="store_true", default=False,
                        help="options platforms: fall back to BinanceUS <symbol>/USDT when the adapter has no OHLCV")
    parser.add_argument("--probe-only", action="store_true", default=False)
    args = parser.parse_args()

    try:
        windows_spec = parse_regime_windows_spec_json(args.regime_windows_spec_json)
        if not windows_spec:
            _emit_error(args, "empty --regime-windows-spec-json")

        df = _fetch_dataframe(args)
        if df is None or len(df) < args.min_bars:
            got = 0 if df is None else len(df)
            _emit_error(args, f"Insufficient data: {got} candles (need {args.min_bars})")

        bundle = compute_regime_bundle(df, windows_spec)

        bar_time = ""
        try:
            idx_last = df.index[-1]
            bar_time = idx_last.isoformat() if hasattr(idx_last, "isoformat") else str(idx_last)
        except Exception:  # noqa: BLE001 - bar_time is informational only
            bar_time = ""

        print(json.dumps({
            "platform": args.platform,
            "symbol": args.symbol,
            "timeframe": args.timeframe,
            "bar_time": bar_time,
            "regime": bundle["regime"],
            "views": bundle["views"],
            "timestamp": datetime.now(timezone.utc).isoformat(),
        }))
    except SystemExit:
        raise
    except Exception as e:  # noqa: BLE001 - subprocess contract: JSON + exit 1
        import traceback

        traceback.print_exc(file=sys.stderr)
        _emit_error(args, str(e))


if __name__ == "__main__":
    main()

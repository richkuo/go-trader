#!/usr/bin/env python3
"""#879: dedicated read-only regime subprocess.

Fetches OHLCV for one (platform, symbol, interval), computes the multi-window
regime payload via shared_tools/regime.py, and emits it. The Go scheduler runs
this ONCE per distinct (symbol, interval, windows-spec) signature per cycle and
injects the payload into every check via --regime-payload-json, so checks no
longer compute regime inline.

Read-only: never places orders. Per-platform fetch mirrors each check script's
exact adapter + method so candles (and therefore labels) are identical to the
pre-migration inline path. Subprocess contract: JSON on stdout even on error,
exit 1 on failure.

Usage:
  fetch_regime.py --platform hyperliquid --symbol BTC --interval 1h \
      --regime-windows-spec-json '{"default":{"classifier":"adx","period":14}}' \
      --ohlcv-limit 200 [--inst-type swap] [--mode paper]
"""
from __future__ import annotations

import argparse
import json
import sys
from pathlib import Path

ROOT = Path(__file__).resolve().parents[1]
sys.path.insert(0, str(ROOT / "shared_tools"))
sys.path.insert(0, str(ROOT))

import pandas as pd  # noqa: E402

from regime import (  # noqa: E402
    compute_multi_regime,
    parse_regime_windows_spec_json,
    required_ohlcv_limit,
)


class SafeEncoder(json.JSONEncoder):
    def default(self, o):
        try:
            import numpy as np

            if isinstance(o, np.integer):
                return int(o)
            if isinstance(o, np.floating):
                return float(o)
            if isinstance(o, np.ndarray):
                return o.tolist()
        except ImportError:
            pass
        return super().default(o)


def _make_dataframe(candles) -> pd.DataFrame:
    df = pd.DataFrame(candles, columns=["timestamp", "open", "high", "low", "close", "volume"])
    for col in ("open", "high", "low", "close", "volume"):
        df[col] = pd.to_numeric(df[col], errors="coerce")
    return df


def compute_payload(df: pd.DataFrame, spec: dict) -> dict:
    """Multi-window payload identical to prepare_check_regime(..., windows_spec=spec)[0]."""
    return compute_multi_regime(df, spec)


def _fetch_dataframe(platform: str, symbol: str, interval: str, limit: int,
                     inst_type: str, mode: str) -> pd.DataFrame:
    """Per-platform OHLCV fetch, mirroring the matching check_*.py exactly."""
    plat = (platform or "").strip().lower()

    if plat in ("binanceus", ""):
        # check_strategy.py spot path: data_fetcher.fetch_ohlcv → DataFrame.
        from data_fetcher import fetch_ohlcv

        return fetch_ohlcv(symbol=symbol, timeframe=interval, limit=limit, store=False)

    if plat == "hyperliquid":
        # SDK clash workaround (see CLAUDE.md): add platforms/hyperliquid first.
        sys.path.insert(0, str(ROOT / "platforms" / "hyperliquid"))
        from adapter import HyperliquidExchangeAdapter

        adapter = HyperliquidExchangeAdapter()
        candles = adapter.get_ohlcv(symbol, interval=interval, limit=limit)
        return _make_dataframe(candles)

    if plat == "okx":
        sys.path.insert(0, str(ROOT / "platforms" / "okx"))
        from adapter import OKXExchangeAdapter

        adapter = OKXExchangeAdapter()
        if (inst_type or "swap").strip().lower() == "swap":
            candles = adapter.get_perp_ohlcv(symbol, interval=interval, limit=limit)
        else:
            candles = adapter.get_ohlcv(symbol, interval=interval, limit=limit)
        return _make_dataframe(candles)

    if plat == "robinhood":
        sys.path.insert(0, str(ROOT / "platforms" / "robinhood"))
        from adapter import RobinhoodExchangeAdapter

        adapter = RobinhoodExchangeAdapter(mode=mode or "paper")
        candles = adapter.get_ohlcv(symbol, interval=interval, limit=limit)
        return _make_dataframe(candles)

    if plat == "topstep":
        sys.path.insert(0, str(ROOT / "platforms" / "topstep"))
        from adapter import TopStepExchangeAdapter

        adapter = TopStepExchangeAdapter(mode=mode or "paper")
        candles = adapter.get_ohlcv(symbol, interval=interval, limit=limit)
        return _make_dataframe(candles)

    raise ValueError(f"unsupported platform for regime fetch: {platform!r}")


def main() -> int:
    p = argparse.ArgumentParser()
    p.add_argument("--platform", required=False, default="")
    p.add_argument("--symbol", required=False, default="")
    p.add_argument("--interval", required=False, default="")
    p.add_argument("--regime-windows-spec-json", default="")
    p.add_argument("--ohlcv-limit", type=int, default=200)
    p.add_argument("--inst-type", default="")
    p.add_argument("--mode", default="paper")
    p.add_argument("--probe-only", action="store_true")
    args = p.parse_args()

    # --probe-only short-circuits before any adapter/network call (probe contract).
    if args.probe_only:
        print(json.dumps({"regime": "", "bar_time": 0}))
        return 0

    try:
        spec = parse_regime_windows_spec_json(args.regime_windows_spec_json or None)
        if not spec:
            print(json.dumps({"regime": "", "bar_time": 0}))
            return 0
        limit = max(int(args.ohlcv_limit or 0), required_ohlcv_limit(windows=spec))
        df = _fetch_dataframe(args.platform, args.symbol, args.interval, limit,
                              args.inst_type, args.mode)
        if df is None or len(df) < 30:
            print(json.dumps({"error": f"insufficient candles: {0 if df is None else len(df)}"}))
            return 1
        payload = compute_payload(df, spec)
        bar_time = int(df["timestamp"].iloc[-1]) if len(df) else 0
        print(json.dumps({"regime": payload, "bar_time": bar_time}, cls=SafeEncoder))
        return 0
    except Exception as exc:  # subprocess contract: JSON on stdout, exit 1
        print(json.dumps({"error": str(exc)}))
        return 1


if __name__ == "__main__":
    sys.exit(main())

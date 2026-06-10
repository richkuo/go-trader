"""Backtest-vs-live parity diff tool (#906 D7.4).

The parity contract between the backtester and the live scheduler is:
same ``compute_*`` function, same window, same params → same decision.
The backtester evaluates a strategy ONCE over the full vectorized frame
and reads decisions row-by-row; the live check scripts re-evaluate the
strategy every cycle over a trailing fetch window (``--ohlcv-limit``,
default 200 in ``shared_scripts/check_strategy.py``) and act on the LAST
closed bar. Any strategy whose bar-N output depends on the frame it was
computed in — full-series normalization, unseeded rolling state, warmup
that never converges — silently diverges between the two paths, and no
existing test catches it because both suites only exercise one path.

This tool replays both paths over the same candles and emits a per-bar
diff of the decision surface:

  • ``signal``          — vectorized value at bar N  vs  last-bar value of a
                          window ENDING at bar N (live semantics).
  • ``open_action``     — same comparison when the strategy emits the
                          open/close-split column.
  • ``close_fraction``  — max across ``close_fraction*`` columns, same
                          comparison.
  • ``regime``          — full-frame ``compute_regime`` label at bar N vs
                          ``latest_regime`` on the trailing window (the
                          per-bar generalization of the last-bar parity
                          test in ``test_backtester_regime.py``).

Usage:
  uv run --no-sync python backtest/parity_diff.py \
      --strategy supertrend --symbol BTC/USDT --timeframe 1h \
      [--since 2024-01-01] [--params '{"period": 10}'] \
      [--registry spot|futures] [--window 200] [--stride 1] \
      [--regime] [--csv /tmp/diff.csv]

Exit code 0 when the paths agree on every compared bar, 1 when any bar
differs (CI-friendly), 2 on usage/data errors.

The per-bar loop re-runs the strategy O(N) times on trailing windows —
this is a debugging tool, not a benchmark; bound the range with --since
or thin the comparison with --stride for long histories.
"""

import argparse
import json
import os
import sys
from typing import Optional

import pandas as pd

sys.path.insert(0, os.path.join(os.path.dirname(__file__), '..', 'shared_tools'))

from registry_loader import load_registry
from backtester import _max_close_fraction_series, _normalize_open_action

# Mirror live: check_strategy.py refuses to evaluate fewer than 30 candles.
LIVE_MIN_CANDLES = 30
# Mirror live: check scripts fetch --ohlcv-limit candles (default 200).
DEFAULT_WINDOW = 200


def _normalize_signal(value) -> int:
    """Collapse a raw strategy signal to the live {-1, 0, 1} domain."""
    try:
        f = float(value)
    except (TypeError, ValueError):
        return 0
    if pd.isna(f):
        return 0
    if f > 0:
        return 1
    if f < 0:
        return -1
    return 0


def _last_bar_decision(result_df: pd.DataFrame) -> dict:
    """Extract the live-path decision surface from a strategy result frame."""
    last = result_df.iloc[-1]
    decision = {"signal": _normalize_signal(last.get("signal", 0))}
    if "open_action" in result_df.columns:
        decision["open_action"] = _normalize_open_action(last.get("open_action"))
    frac_series = _max_close_fraction_series(result_df)
    if frac_series.any():
        decision["close_fraction"] = float(frac_series.iloc[-1])
    elif any(c == "close_fraction" or c.startswith("close_fraction:")
             for c in result_df.columns):
        decision["close_fraction"] = 0.0
    return decision


def _full_frame_decisions(result_df: pd.DataFrame) -> pd.DataFrame:
    """Extract the backtest-path decision surface for every bar."""
    out = pd.DataFrame(index=result_df.index)
    out["signal"] = result_df.get(
        "signal", pd.Series(0, index=result_df.index)
    ).map(_normalize_signal)
    if "open_action" in result_df.columns:
        out["open_action"] = result_df["open_action"].map(_normalize_open_action)
    if any(c == "close_fraction" or c.startswith("close_fraction:")
           for c in result_df.columns):
        out["close_fraction"] = _max_close_fraction_series(result_df)
    return out


def compute_parity_frame(
    df: pd.DataFrame,
    strategy_name: str,
    params: Optional[dict] = None,
    registry: str = "spot",
    window: Optional[int] = DEFAULT_WINDOW,
    stride: int = 1,
    regime_enabled: bool = False,
    regime_period: int = 14,
    regime_adx_threshold: float = 20.0,
) -> pd.DataFrame:
    """Replay both decision paths over ``df`` and return the per-bar diff.

    Returns one row per compared bar with ``bt_*`` (vectorized full-frame)
    and ``live_*`` (trailing-window last-bar) columns plus a ``match``
    bool. Comparison starts at the first bar where the trailing window is
    full (``window`` bars, or ``LIVE_MIN_CANDLES`` for expanding mode) so
    every live evaluation sees the same window length it would in
    production — earlier bars would diff on warmup, not on parity.
    """
    if stride < 1:
        raise ValueError("stride must be >= 1")
    if window is not None and window < LIVE_MIN_CANDLES:
        raise ValueError(f"window must be >= {LIVE_MIN_CANDLES} (live minimum)")
    reg = load_registry(registry)
    full_result = reg.apply_strategy(strategy_name, df.copy(), dict(params or {}))
    bt = _full_frame_decisions(full_result)

    regime_full = None
    if regime_enabled:
        from regime import compute_regime, latest_regime
        regime_full = compute_regime(
            df, period=regime_period, adx_threshold=regime_adx_threshold
        )["regime"]

    start = (window - 1) if window is not None else (LIVE_MIN_CANDLES - 1)
    rows = []
    for i in range(start, len(df), stride):
        lo = max(0, i + 1 - window) if window is not None else 0
        win = df.iloc[lo:i + 1].copy()
        live = _last_bar_decision(
            reg.apply_strategy(strategy_name, win, dict(params or {}))
        )
        row = {
            "ts": df.index[i],
            "bt_signal": int(bt["signal"].iloc[i]),
            "live_signal": live["signal"],
        }
        match = row["bt_signal"] == row["live_signal"]
        if "open_action" in bt.columns:
            row["bt_open_action"] = bt["open_action"].iloc[i]
            row["live_open_action"] = live.get("open_action", "none")
            match = match and row["bt_open_action"] == row["live_open_action"]
        if "close_fraction" in bt.columns:
            row["bt_close_fraction"] = float(bt["close_fraction"].iloc[i])
            row["live_close_fraction"] = float(live.get("close_fraction", 0.0))
            match = match and abs(
                row["bt_close_fraction"] - row["live_close_fraction"]
            ) < 1e-9
        if regime_enabled:
            from regime import latest_regime
            row["bt_regime"] = str(regime_full.iloc[i])
            row["live_regime"] = str(latest_regime(
                win, period=regime_period, adx_threshold=regime_adx_threshold
            )["regime"])
            match = match and row["bt_regime"] == row["live_regime"]
        row["match"] = match
        rows.append(row)
    return pd.DataFrame(rows)


def summarize(frame: pd.DataFrame) -> dict:
    """Aggregate a parity frame into a result summary."""
    if frame.empty:
        return {"bars_compared": 0, "mismatches": 0, "clean": True}
    mismatched = frame[~frame["match"]]
    summary = {
        "bars_compared": int(len(frame)),
        "mismatches": int(len(mismatched)),
        "clean": bool(mismatched.empty),
    }
    if not mismatched.empty:
        summary["first_mismatch"] = str(mismatched.iloc[0]["ts"])
        summary["last_mismatch"] = str(mismatched.iloc[-1]["ts"])
    return summary


def main(argv: Optional[list] = None) -> int:
    parser = argparse.ArgumentParser(
        description="Per-bar backtest-vs-live decision diff (#906 D7.4)"
    )
    parser.add_argument("--strategy", required=True)
    parser.add_argument("--symbol", default="BTC/USDT")
    parser.add_argument("--timeframe", default="1h")
    parser.add_argument("--since", default=None,
                        help="Start date YYYY-MM-DD (bounds the replay)")
    parser.add_argument("--params", default=None,
                        help="JSON dict of strategy param overrides")
    parser.add_argument("--registry", choices=["spot", "futures"],
                        default="spot")
    parser.add_argument("--window", type=int, default=DEFAULT_WINDOW,
                        help=f"Trailing candle window for the live path "
                             f"(default {DEFAULT_WINDOW}, matching the check "
                             f"scripts' --ohlcv-limit); 0 = expanding window")
    parser.add_argument("--stride", type=int, default=1,
                        help="Compare every Nth bar (speeds up long ranges)")
    parser.add_argument("--regime", action="store_true",
                        help="Also diff the regime label per bar")
    parser.add_argument("--regime-period", type=int, default=14)
    parser.add_argument("--regime-adx-threshold", type=float, default=20.0)
    parser.add_argument("--csv", default=None,
                        help="Write the full per-bar frame to this CSV path")
    parser.add_argument("--max-print", type=int, default=20,
                        help="Max mismatching rows printed to stdout")
    args = parser.parse_args(argv)

    params = None
    if args.params:
        try:
            params = json.loads(args.params)
        except json.JSONDecodeError as e:
            print(f"--params is not valid JSON: {e}", file=sys.stderr)
            return 2
        if not isinstance(params, dict):
            print("--params must be a JSON object", file=sys.stderr)
            return 2

    from data_fetcher import load_cached_data
    df = load_cached_data(args.symbol, args.timeframe, start_date=args.since)
    if df is None or df.empty:
        print(f"No cached data for {args.symbol} {args.timeframe} — run a "
              f"backtest first to populate the cache.", file=sys.stderr)
        return 2

    window = args.window if args.window and args.window > 0 else None
    frame = compute_parity_frame(
        df, args.strategy, params=params, registry=args.registry,
        window=window, stride=args.stride, regime_enabled=args.regime,
        regime_period=args.regime_period,
        regime_adx_threshold=args.regime_adx_threshold,
    )
    result = summarize(frame)

    if args.csv:
        frame.to_csv(args.csv, index=False)
        print(f"Per-bar frame written to {args.csv}")

    print(f"\nParity diff: {args.strategy} on {args.symbol} {args.timeframe} "
          f"(window={'expanding' if window is None else window}, "
          f"stride={args.stride})")
    print(f"  Bars compared: {result['bars_compared']}")
    print(f"  Mismatches:    {result['mismatches']}")
    if result["clean"]:
        print("  CLEAN — backtest and live paths agree on every compared bar.")
        return 0

    print(f"  First mismatch: {result['first_mismatch']}")
    print(f"  Last mismatch:  {result['last_mismatch']}")
    mismatched = frame[~frame["match"]]
    with pd.option_context("display.max_columns", None, "display.width", 200):
        print(mismatched.head(args.max_print).to_string(index=False))
    if len(mismatched) > args.max_print:
        print(f"  … {len(mismatched) - args.max_print} more "
              f"(use --csv for the full frame)")
    return 1


if __name__ == "__main__":
    sys.exit(main())

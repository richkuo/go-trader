#!/usr/bin/env python3
"""
Run backtests with multiple strategies across multiple assets and timeframes.
Main entry point for strategy evaluation.
"""

import sys
import os
import argparse
from typing import List, Optional

import numpy as np
import pandas as pd

# shared_tools is needed for data_fetcher; the strategy registry is loaded
# dynamically per-registry via registry_loader.
sys.path.insert(0, os.path.join(os.path.dirname(__file__), '..', 'shared_tools'))

from atr import ensure_atr_indicator
from data_fetcher import load_cached_data
from htf_filter import get_default_htf, apply_htf_filter  # noqa: E402
from registry_loader import load_registry
from backtester import Backtester, format_results
from optimizer import walk_forward_optimize, DEFAULT_PARAM_RANGES
from reporter import (
    format_single_report, format_comparison_report,
    format_multi_asset_report, format_walk_forward_report,
    generate_full_report,
)


def _htf_trend_series(symbol: str, timeframe: str, ltf_index: pd.Index,
                      ema_period: int = 50) -> pd.Series:
    """Compute the HTF trend (1/-1/0) aligned to each LTF bar.

    Live scheduler uses ``htf_trend_filter`` which fetches the HTF series at
    request time. Backtest mirrors the same EMA logic (alpha = 2/(N+1),
    matching ``shared_tools/htf_filter._compute_ema``) against cached HTF
    OHLCV, then forward-fills onto the LTF bar index so each LTF bar sees
    the most recently closed HTF bar — same temporal semantics as live
    (issue #304 M2).
    """
    htf = get_default_htf(timeframe)
    htf_df = load_cached_data(symbol, htf)
    if htf_df.empty or len(htf_df) < ema_period:
        # No HTF data → return neutral so signals pass through unfiltered
        # (same fail-open behavior as live ``htf_trend_filter`` on error).
        return pd.Series(0, index=ltf_index, dtype=int)

    closes = htf_df["close"].astype(float)
    ema = closes.ewm(span=ema_period, adjust=False).mean()
    trend = pd.Series(
        np.where(closes > ema, 1, np.where(closes < ema, -1, 0)),
        index=htf_df.index,
        dtype=int,
    )
    return trend.reindex(ltf_index, method="ffill").fillna(0).astype(int)


def _apply_htf_filter_to_df(df: pd.DataFrame, symbol: str,
                            timeframe: str) -> pd.DataFrame:
    """Filter ``df['signal']`` in place against the HTF trend."""
    if "signal" not in df.columns:
        return df
    trend = _htf_trend_series(symbol, timeframe, df.index)
    df = df.copy()
    df["signal"] = [
        apply_htf_filter(int(s), int(t))
        for s, t in zip(df["signal"].fillna(0).astype(int), trend)
    ]
    return df


DEFAULT_SYMBOLS = ["BTC/USDT", "ETH/USDT", "SOL/USDT", "BNB/USDT"]
DEFAULT_TIMEFRAMES = ["4h", "1d"]


def load_strategy_config(config_path: str, strategy_id: str) -> dict:
    """Load a single strategy's refs from a live go-trader config (#641).

    Reads the v13+ config at ``config_path``, finds the strategy with
    ``id == strategy_id``, and returns kwargs ready to splat into
    ``Backtester(**kwargs)`` plus the open name needed for the upstream
    ``apply_strategy`` call. Lets operators backtest the exact live
    config without translating shapes.

    Returns ``{"open_strategy": {...}, "close_strategies": [...]}``.

    Raises ValueError when the config is pre-v13 (legacy flat shape) or
    the strategy ID is not found — the caller should run the live
    binary's migration first.
    """
    import json as _json
    with open(config_path) as fh:
        cfg = _json.load(fh)
    version = int(cfg.get("config_version", 0) or 0)
    if version < 13:
        raise ValueError(
            f"{config_path}: config_version={version} predates the co-located "
            f"strategy ref shape (#640). Run go-trader once against this file "
            f"to migrate it, then retry."
        )
    for sc in cfg.get("strategies", []) or []:
        if sc.get("id") != strategy_id:
            continue
        open_ref = sc.get("open_strategy") or {}
        if not isinstance(open_ref, dict) or not open_ref.get("name"):
            raise ValueError(
                f"{config_path}: strategy {strategy_id!r} has no open_strategy.name; "
                f"the migrated config should always populate it."
            )
        close_refs = []
        for ref in sc.get("close_strategies", []) or []:
            if isinstance(ref, dict) and ref.get("name"):
                close_refs.append({"name": ref["name"], "params": dict(ref.get("params") or {})})
        return {
            "open_strategy": {
                "name": open_ref["name"],
                "params": dict(open_ref.get("params") or {}),
            },
            "close_strategies": close_refs,
        }
    raise ValueError(
        f"{config_path}: no strategy with id={strategy_id!r}. "
        f"Available: {[s.get('id') for s in cfg.get('strategies', []) or []]}"
    )


def run_single_backtest(
    strategy_name: str = "sma_crossover",
    symbol: str = "BTC/USDT",
    timeframe: str = "1d",
    since: str = "2022-01-01",
    capital: float = 1000.0,
    params: dict = None,
    registry: str = "spot",
    platform: str = "binanceus",
    htf_filter: bool = False,
    close_strategies: Optional[List[dict]] = None,
    regime_enabled: bool = False,
    regime_period: int = 14,
    regime_adx_threshold: float = 20.0,
    allowed_regimes: Optional[List[str]] = None,
) -> Optional[dict]:
    """Run a single backtest and print results.

    ``registry`` selects the strategy registry (``"spot"`` or ``"futures"``).
    ``platform`` selects the exchange fee model (``"binanceus"``,
    ``"hyperliquid"``, ``"robinhood"``, ``"luno"``, ``"okx"``,
    ``"okx-perps"``), matching ``scheduler/fees.go:CalculatePlatformSpotFee``.
    ``close_strategies`` is an optional list of co-located close-evaluator
    refs (``[{"name": str, "params": dict}, ...]``) from the close registry
    (#511, #641); each runs per-bar against the simulated position. Backtest
    granularity is bar-level so live intra-bar trigger races (e.g. HL
    stop-loss OIDs) are not simulated.
    """
    reg = load_registry(registry)
    strat = reg.STRATEGY_REGISTRY.get(strategy_name)
    if not strat:
        print(f"Unknown strategy '{strategy_name}' in '{registry}' registry")
        print(f"Available: {reg.list_strategies()}")
        return None

    strat_params = params or strat["default_params"]
    print(f"\n▶ Strategy: {strat['description']}")
    print(f"  Params: {strat_params}")
    print(f"  Symbol: {symbol} | Timeframe: {timeframe} | Since: {since}")
    if close_strategies:
        print(f"  Close strategies: {[r.get('name') for r in close_strategies]}")

    df = load_cached_data(symbol, timeframe, start_date=since)
    if df.empty:
        print("No data available!")
        return None

    print(f"  Data: {len(df)} candles from {df.index[0]} to {df.index[-1]}")

    df_signals = reg.apply_strategy(strategy_name, df, strat_params)

    # Mirror the runtime check-script contract: inject ATR(14) when the
    # open strategy doesn't emit `atr`, so close evaluators that require
    # `entry_atr` (tiered_tp_atr) and `market.atr` (tiered_tp_atr_live)
    # see consistent volatility input. Idempotent when `atr` already exists.
    if close_strategies:
        df_signals = ensure_atr_indicator(df_signals)

    if htf_filter:
        df_signals = _apply_htf_filter_to_df(df_signals, symbol, timeframe)
        print(f"  HTF filter: applied (HTF={get_default_htf(timeframe)})")

    bt = Backtester(
        initial_capital=capital, platform=platform,
        open_strategy={"name": strategy_name, "params": dict(strat_params or {})},
        close_strategies=close_strategies,
        regime_enabled=regime_enabled,
        regime_period=regime_period,
        regime_adx_threshold=regime_adx_threshold,
        allowed_regimes=allowed_regimes,
    )
    results = bt.run(
        df_signals,
        strategy_name=strategy_name,
        symbol=symbol,
        timeframe=timeframe,
        params=strat_params,
    )

    print(format_single_report(results))
    return results


def run_all_strategies(
    symbol: str = "BTC/USDT",
    timeframe: str = "1d",
    since: str = "2022-01-01",
    capital: float = 1000.0,
    strategies: Optional[List[str]] = None,
    registry: str = "spot",
    platform: str = "binanceus",
    htf_filter: bool = False,
    close_strategies: Optional[List[dict]] = None,
    regime_enabled: bool = False,
    regime_period: int = 14,
    regime_adx_threshold: float = 20.0,
    allowed_regimes: Optional[List[str]] = None,
) -> list:
    """Run multiple strategies on one asset and compare."""
    reg = load_registry(registry)
    strat_list = strategies or reg.list_strategies()
    print(f"\n{'#'*60}")
    print(f"  RUNNING {len(strat_list)} STRATEGIES ({registry} / {platform})")
    print(f"  {symbol} | {timeframe} | since {since} | ${capital:,.0f}")
    print(f"{'#'*60}")

    all_results = []
    for name in strat_list:
        result = run_single_backtest(
            name, symbol, timeframe, since, capital,
            registry=registry, platform=platform, htf_filter=htf_filter,
            close_strategies=close_strategies,
            regime_enabled=regime_enabled, regime_period=regime_period,
            regime_adx_threshold=regime_adx_threshold,
            allowed_regimes=allowed_regimes,
        )
        if result:
            all_results.append(result)

    if all_results:
        print(format_comparison_report(all_results))

    return all_results


def run_multi_asset(
    strategies: Optional[List[str]] = None,
    symbols: Optional[List[str]] = None,
    timeframe: str = "1d",
    since: str = "2022-01-01",
    capital: float = 1000.0,
    registry: str = "spot",
    platform: str = "binanceus",
    htf_filter: bool = False,
    close_strategies: Optional[List[dict]] = None,
    regime_enabled: bool = False,
    regime_period: int = 14,
    regime_adx_threshold: float = 20.0,
    allowed_regimes: Optional[List[str]] = None,
) -> dict:
    """Run strategies across multiple assets."""
    reg = load_registry(registry)
    strat_list = strategies or reg.list_strategies()
    sym_list = symbols or DEFAULT_SYMBOLS

    print(f"\n{'#'*60}")
    print(f"  MULTI-ASSET BACKTEST ({registry} / {platform})")
    print(f"  Strategies: {len(strat_list)} | Assets: {len(sym_list)}")
    print(f"  Timeframe: {timeframe} | Since: {since}")
    print(f"{'#'*60}")

    results_by_asset = {}
    for symbol in sym_list:
        print(f"\n{'─'*40}")
        print(f"  Asset: {symbol}")
        print(f"{'─'*40}")
        results_by_asset[symbol] = []
        for strat_name in strat_list:
            result = run_single_backtest(
                strat_name, symbol, timeframe, since, capital,
                registry=registry, platform=platform, htf_filter=htf_filter,
                close_strategies=close_strategies,
                regime_enabled=regime_enabled, regime_period=regime_period,
                regime_adx_threshold=regime_adx_threshold,
                allowed_regimes=allowed_regimes,
            )
            if result:
                results_by_asset[symbol].append(result)

    print(format_multi_asset_report(results_by_asset))
    return results_by_asset


def run_walk_forward(
    strategy_name: str,
    symbol: str = "BTC/USDT",
    timeframe: str = "1d",
    since: str = "2020-01-01",
    n_splits: int = 5,
    capital: float = 1000.0,
    registry: str = "spot",
    platform: str = "binanceus",
    regime_enabled: bool = False,
    regime_period: int = 14,
    regime_adx_threshold: float = 20.0,
    allowed_regimes: Optional[List[str]] = None,
) -> Optional[dict]:
    """Run walk-forward optimization for a strategy."""
    reg = load_registry(registry)
    strat = reg.STRATEGY_REGISTRY.get(strategy_name)
    if not strat:
        print(f"Unknown strategy '{strategy_name}' in '{registry}' registry")
        return None

    param_ranges = DEFAULT_PARAM_RANGES.get(strategy_name)
    if not param_ranges:
        # Fall back to a single-point grid built from default_params with a
        # clear warning, instead of silently returning None.
        print(f"[warn] No DEFAULT_PARAM_RANGES for '{strategy_name}' — "
              f"using single-point grid from default_params. "
              f"Add a range entry in optimizer.DEFAULT_PARAM_RANGES for "
              f"meaningful walk-forward results.")
        param_ranges = {k: [v] for k, v in strat["default_params"].items()}
        if not param_ranges:
            print(f"[warn] '{strategy_name}' has no default_params either — skipping.")
            return None

    df = load_cached_data(symbol, timeframe, start_date=since)
    if df.empty:
        print("No data available!")
        return None

    result = walk_forward_optimize(
        df, strategy_name, param_ranges,
        n_splits=n_splits,
        initial_capital=capital,
        symbol=symbol,
        timeframe=timeframe,
        registry=registry,
        platform=platform,
        verbose=True,
        regime_enabled=regime_enabled,
        regime_period=regime_period,
        regime_adx_threshold=regime_adx_threshold,
        allowed_regimes=allowed_regimes,
    )

    print(format_walk_forward_report(result))
    return result


def _build_parser() -> argparse.ArgumentParser:
    parser = argparse.ArgumentParser(description="Crypto Trading Bot — Backtester")
    parser.add_argument("--strategy", "-s", default="all",
                        help="Strategy name or 'all'")
    parser.add_argument("--registry", choices=["spot", "futures"], default="spot",
                        help="Strategy registry to load (spot or futures)")
    parser.add_argument("--platform",
                        choices=["binanceus", "hyperliquid", "robinhood",
                                 "luno", "okx", "okx-perps"],
                        default="binanceus",
                        help="Exchange fee model (matches fees.go)")
    parser.add_argument("--symbol", default="BTC/USDT",
                        help="Trading pair")
    parser.add_argument("--symbols", nargs="+", default=None,
                        help="Multiple trading pairs for multi-asset mode")
    parser.add_argument("--timeframe", "-tf", default="1d",
                        help="Candle timeframe (1h, 4h, 1d)")
    parser.add_argument("--since", default="2022-01-01",
                        help="Start date")
    parser.add_argument("--capital", type=float, default=1000.0,
                        help="Starting capital")
    parser.add_argument("--mode", choices=["single", "compare", "multi", "optimize"],
                        default="compare",
                        help="Run mode: single/compare/multi/optimize")
    parser.add_argument("--splits", type=int, default=5,
                        help="Walk-forward splits (optimize mode)")
    parser.add_argument("--htf-filter", action="store_true",
                        help="Apply HTF trend filter (matches live "
                             "shared_tools/htf_filter.py); 50-EMA on the "
                             "default HTF for the chosen timeframe.")
    parser.add_argument("--close-strategy", action="append", dest="close_strategies",
                        default=None, metavar="REF",
                        help="Close-evaluator ref. Two accepted shapes (#641):\n"
                             "  - bare name: --close-strategy tp_at_pct\n"
                             "  - JSON ref:  --close-strategy '{\"name\":\"tp_at_pct\",\"params\":{\"pct\":0.03}}'\n"
                             "Repeat for multiple. Each runs per-bar against the simulated position; "
                             "max close_fraction wins. Replaces the pre-#641 --close-strategy NAME + "
                             "--close-params JSON pair.")
    parser.add_argument("--config", default=None,
                        help="Path to a live go-trader config.json. Loads a single strategy by "
                             "--strategy ID and uses its open_strategy/close_strategies refs verbatim "
                             "for the backtest. Lets you backtest a live config without reshaping (#641).")
    parser.add_argument("--regime-enabled", action="store_true", default=False,
                        help="Enable market regime detection. Injects vectorized regime "
                             "column from shared_tools/regime.py before the per-bar loop, "
                             "matching the live check-script contract (#482).")
    parser.add_argument("--regime-period", type=int, default=14,
                        help="ADX lookback period for regime detection (default: 14).")
    parser.add_argument("--regime-adx-threshold", type=float, default=20.0,
                        help="ADX threshold below which market is 'ranging' (default: 20.0).")
    parser.add_argument("--allowed-regimes", action="append", dest="allowed_regimes",
                        default=None, choices=["trending_up", "trending_down", "ranging"],
                        metavar="LABEL",
                        help="Regime label to allow entries for (repeat for multiple). "
                             "Empty = allow all. Valid: trending_up, trending_down, ranging.")
    return parser


def _parse_close_strategy_arg(raw: str) -> dict:
    """Parse a --close-strategy CLI value into a {name, params} ref (#641).

    Accepts two shapes for ergonomics:
      - bare name (no leading '{'): wraps as {"name": <raw>, "params": {}}
      - JSON object: parsed as-is, requires "name" key, normalized "params"
    """
    import json as _json
    s = raw.strip()
    if not s.startswith(("{", "[")):
        return {"name": s, "params": {}}
    try:
        ref = _json.loads(s)
    except _json.JSONDecodeError as exc:
        raise SystemExit(f"--close-strategy not valid JSON: {exc}\nGot: {raw}")
    if not isinstance(ref, dict):
        raise SystemExit(f"--close-strategy JSON must be an object, got {type(ref).__name__}")
    name = (ref.get("name") or "").strip()
    if not name:
        raise SystemExit(f"--close-strategy ref missing 'name': {raw}")
    return {"name": name, "params": dict(ref.get("params") or {})}


def main():
    args = _build_parser().parse_args()

    close_refs = None
    if args.close_strategies:
        close_refs = [_parse_close_strategy_arg(v) for v in args.close_strategies]

    # #641: --config loads a single strategy by ID and uses its refs directly.
    open_params: Optional[dict] = None
    if args.config:
        # --config loads exactly one strategy; non-single modes would silently
        # ignore the loaded refs for every strategy except the one matching
        # --strategy. Reject upfront instead of producing misleading reports.
        if args.mode != "single":
            print("--config is only valid with --mode single (loads one strategy by --strategy <id>)")
            sys.exit(1)
        live_kwargs = load_strategy_config(args.config, args.strategy)
        # Live config refs take precedence; --close-strategy on top is rejected
        # to avoid silent overrides.
        if close_refs:
            print("--close-strategy is not allowed alongside --config (refs come from the live config)")
            sys.exit(1)
        close_refs = live_kwargs["close_strategies"]
        # Open strategy name + params come from the live config. Threading
        # params through to run_single_backtest is required — without it,
        # run_single_backtest falls back to the registry default_params and
        # silently ignores per-strategy params from the live config (#643 review #1).
        args.strategy = live_kwargs["open_strategy"]["name"]
        open_params = dict(live_kwargs["open_strategy"]["params"]) or None

    reg = load_registry(args.registry)

    if args.mode == "single":
        if args.strategy == "all":
            print("Specify a strategy for single mode: --strategy <name>")
            sys.exit(1)
        run_single_backtest(args.strategy, args.symbol, args.timeframe,
                            args.since, args.capital,
                            params=open_params,
                            registry=args.registry, platform=args.platform,
                            htf_filter=args.htf_filter,
                            close_strategies=close_refs,
                            regime_enabled=args.regime_enabled,
                            regime_period=args.regime_period,
                            regime_adx_threshold=args.regime_adx_threshold,
                            allowed_regimes=args.allowed_regimes)

    elif args.mode == "compare":
        strategies = None if args.strategy == "all" else [args.strategy]
        run_all_strategies(args.symbol, args.timeframe, args.since, args.capital,
                           strategies,
                           registry=args.registry, platform=args.platform,
                           htf_filter=args.htf_filter,
                           close_strategies=close_refs,
                           regime_enabled=args.regime_enabled,
                           regime_period=args.regime_period,
                           regime_adx_threshold=args.regime_adx_threshold,
                           allowed_regimes=args.allowed_regimes)

    elif args.mode == "multi":
        strategies = None if args.strategy == "all" else [args.strategy]
        symbols = args.symbols or DEFAULT_SYMBOLS
        run_multi_asset(strategies, symbols, args.timeframe, args.since,
                        args.capital,
                        registry=args.registry, platform=args.platform,
                        htf_filter=args.htf_filter,
                        close_strategies=close_refs,
                        regime_enabled=args.regime_enabled,
                        regime_period=args.regime_period,
                        regime_adx_threshold=args.regime_adx_threshold,
                        allowed_regimes=args.allowed_regimes)

    elif args.mode == "optimize":
        if args.strategy == "all":
            for strat in reg.list_strategies():
                run_walk_forward(strat, args.symbol, args.timeframe,
                                 args.since, args.splits, args.capital,
                                 registry=args.registry, platform=args.platform,
                                 regime_enabled=args.regime_enabled,
                                 regime_period=args.regime_period,
                                 regime_adx_threshold=args.regime_adx_threshold,
                                 allowed_regimes=args.allowed_regimes)
        else:
            run_walk_forward(args.strategy, args.symbol, args.timeframe,
                             args.since, args.splits, args.capital,
                             registry=args.registry, platform=args.platform,
                             regime_enabled=args.regime_enabled,
                             regime_period=args.regime_period,
                             regime_adx_threshold=args.regime_adx_threshold,
                             allowed_regimes=args.allowed_regimes)


if __name__ == "__main__":
    main()

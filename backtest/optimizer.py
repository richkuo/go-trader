"""
Walk-forward optimization framework to prevent overfitting.
Splits data into rolling in-sample/out-of-sample windows,
optimizes parameters on in-sample, validates on out-of-sample.
"""

import sys
import os
import json
import itertools
from typing import Dict, List, Optional, Any, Tuple
from datetime import timedelta

import numpy as np
import pandas as pd

from registry_loader import load_registry
from backtester import Backtester
# backtester puts shared_tools on sys.path; atr lives there.
from atr import ensure_atr_indicator


_EXPECTED_FOLD_ERRORS = (KeyError, ValueError, TypeError, IndexError, ZeroDivisionError)


def generate_param_grid(param_ranges: Dict[str, list]) -> List[dict]:
    """Generate all parameter combinations from ranges."""
    keys = list(param_ranges.keys())
    values = list(param_ranges.values())
    combos = list(itertools.product(*values))
    return [dict(zip(keys, combo)) for combo in combos]


def warmup_exit_long_entry(warmup_with_signal: pd.DataFrame,
                             slippage_pct: float) -> Optional[dict]:
    """Walk through warmup signals to find whether the strategy ends the
    warmup period already long, and if so at what effective entry price.

    Mirrors Backtester's execution model: ``signal`` is shifted by one bar
    (signal at bar t fills at bar t+1's open), slippage is added on entry.

    Returns ``{"entry_price": float, "entry_date": idx}`` when the warmup
    ends long, or ``None`` when flat. When the warmup frame carries an
    ``atr`` column the seed also stamps ``entry_atr`` (the fill-bar ATR,
    same plausibility guard as ``Backtester._stamp_entry_atr``) and
    ``high_water`` (max close since entry) so the carried position is
    managed by ATR-based close stacks and trailing stops exactly like a
    mid-window open — without them every ATR exit silently no-ops on the
    seeded position for its entire lifetime. The caller passes the dict to
    ``Backtester.run(starting_long=...)`` so a SELL in the train window
    actually closes the warmup position rather than being silently dropped.
    """
    if len(warmup_with_signal) == 0 or "signal" not in warmup_with_signal.columns:
        return None

    shifted = warmup_with_signal.copy()
    shifted["signal"] = shifted["signal"].shift(1).fillna(0)
    has_open = "open" in shifted.columns

    in_position = False
    entry_price = None
    entry_date = None
    high_water = 0.0
    for idx, row in shifted.iterrows():
        fill_price = row["open"] if has_open else row["close"]
        sig = row["signal"]
        if sig == 1 and not in_position:
            entry_price = fill_price * (1 + slippage_pct)
            entry_date = idx
            in_position = True
            high_water = float(row["close"])
        elif sig == -1 and in_position:
            in_position = False
            entry_price = None
            entry_date = None
            high_water = 0.0
        elif in_position:
            high_water = max(high_water, float(row["close"]))

    if in_position and entry_price is not None:
        seed = {"entry_price": entry_price, "entry_date": entry_date,
                "high_water": high_water}
        if "atr" in shifted.columns:
            try:
                atr_val = float(shifted["atr"].loc[entry_date])
            except (KeyError, TypeError, ValueError):
                atr_val = 0.0
            if atr_val > 0 and atr_val <= 0.5 * entry_price:
                seed["entry_atr"] = atr_val
        return seed
    return None


def max_indicator_lookback(param_ranges: Dict[str, list]) -> int:
    """Heuristic warmup size for walk-forward folds.

    Scans the integer values in the parameter grid and returns the largest.
    Period/lookback-style params are ints (``fast_period``, ``slow_period``,
    ``atr_period``, ``swing_lookback``, etc.) and dominate warmup cost;
    non-integer params (multipliers, thresholds) are ignored since they
    don't add history requirements.

    Returns 0 if no integer params found — matches the pre-warmup behavior
    for grids like ``vwap_reversion`` that only tune std-dev thresholds.
    """
    max_lb = 0
    for values in param_ranges.values():
        for v in values:
            if isinstance(v, int) and v > max_lb:
                max_lb = v
    return max_lb


# ---------------------------------------------------------------------------
# Close-stack co-optimization (#996). A "close stack" is one complete exit
# configuration the Backtester can run: an optional close-evaluator ref plus
# the mutually-exclusive backtester-level ATR stop owners. The grid sweeps
# whole stacks jointly with open-strategy params so walk-forward selection
# picks (entry, exit) pairs instead of freezing one side.
# ---------------------------------------------------------------------------

ATR_CLOSE_WARMUP = 14  # standard ATR(14) priming bars for exit evaluation

_CLOSE_STACK_SPEC_KEYS = {"close", "stop_loss_atr_mult", "trailing_stop_atr_mult"}


def _fmt_num(v) -> str:
    """Compact float formatting for stack labels (1.0 -> '1', 1.50 -> '1.5')."""
    return f"{v:g}"


def close_stack_label(stack: dict) -> str:
    """Human-readable one-line label for a close stack (fold reports, summary)."""
    parts = []
    for ref in stack.get("close_strategies") or []:
        params = ref.get("params") or {}
        tiers = params.get("tp_tiers")
        if tiers:
            ladder = ",".join(
                f"{_fmt_num(t.get('atr_multiple', t.get('profit_pct', '?')))}x"
                f":{_fmt_num(t.get('close_fraction', '?'))}"
                for t in tiers
            )
            extra = {k: v for k, v in params.items() if k != "tp_tiers"}
        else:
            ladder = ""
            extra = dict(params)
        suffix = "".join(f" {k}={v}" for k, v in sorted(extra.items()))
        parts.append(f"{ref['name']}[{ladder}]{suffix}" if ladder
                     else f"{ref['name']}{suffix}")
    if stack.get("stop_loss_atr_mult"):
        parts.append(f"sl_atr={_fmt_num(stack['stop_loss_atr_mult'])}")
    if stack.get("trailing_stop_atr_mult"):
        parts.append(f"trail_atr={_fmt_num(stack['trailing_stop_atr_mult'])}")
    return " ".join(parts) if parts else "baseline"


def generate_close_stack_grid(specs: List[dict]) -> List[dict]:
    """Expand close-stack sweep specs into complete stack candidates.

    Each spec is a dict with keys:
      close: {"name": str, "params": {param: [candidate values]}} or None.
             Param candidates may be structured (a whole tp_tiers ladder is
             ONE candidate value); the cartesian product runs across params.
      stop_loss_atr_mult / trailing_stop_atr_mult: lists of candidate values
             (None = no stop). Defaults to [None] when omitted.

    The two stop owners are mutually exclusive (mirrors the live config's
    exclusive stop fields); a spec that would produce a combo with both set
    raises. Stacks are deduped across specs by canonical JSON.

    Returns [{"label", "close_strategies", "stop_loss_atr_mult",
    "trailing_stop_atr_mult"}, ...].
    """
    stacks: List[dict] = []
    seen = set()
    for i, spec in enumerate(specs):
        if not isinstance(spec, dict):
            raise ValueError(f"close-stack spec {i} must be a dict, got {type(spec).__name__}")
        unknown = set(spec) - _CLOSE_STACK_SPEC_KEYS
        if unknown:
            raise ValueError(
                f"close-stack spec {i} has unknown keys {sorted(unknown)}; "
                f"allowed: {sorted(_CLOSE_STACK_SPEC_KEYS)}")
        close = spec.get("close")
        if close is not None and (not isinstance(close, dict) or not close.get("name")):
            raise ValueError(f"close-stack spec {i}: 'close' needs a 'name'")
        param_ranges = dict((close or {}).get("params") or {})
        for k, v in param_ranges.items():
            if not isinstance(v, list) or not v:
                raise ValueError(
                    f"close-stack spec {i}: close param {k!r} must be a "
                    f"non-empty list of candidate values")
            # Classic footgun: one ladder passed where a list of candidate
            # ladders is expected — each tp_tiers candidate is itself a list.
            if k == "tp_tiers" and not all(isinstance(c, list) for c in v):
                raise ValueError(
                    f"close-stack spec {i}: tp_tiers candidates must each be "
                    f"a non-empty list of tier dicts (a whole ladder is ONE "
                    f"candidate value — wrap a single ladder as [ladder])")
        close_combos = generate_param_grid(param_ranges) if close else [None]
        sl_values = spec.get("stop_loss_atr_mult", [None])
        trail_values = spec.get("trailing_stop_atr_mult", [None])
        for close_params, sl, trail in itertools.product(close_combos, sl_values, trail_values):
            if sl and trail:
                raise ValueError(
                    f"close-stack spec {i} produces a combo with both "
                    f"stop_loss_atr_mult={sl} and trailing_stop_atr_mult={trail}; "
                    f"the stop owners are mutually exclusive — split into "
                    f"separate specs")
            stack = {
                "close_strategies": (
                    [{"name": close["name"], "params": close_params}]
                    if close else []
                ),
                "stop_loss_atr_mult": sl,
                "trailing_stop_atr_mult": trail,
            }
            key = json.dumps(stack, sort_keys=True, default=str)
            if key in seen:
                continue
            seen.add(key)
            stack["label"] = close_stack_label(stack)
            stacks.append(stack)
    if not stacks:
        raise ValueError("close-stack specs expanded to an empty grid")
    return stacks


def _result_metric(result: dict, metric: str) -> float:
    """Extract the optimization metric from a Backtester result.

    ``dd_adjusted_return`` is derived (return / |max DD|, the #963 DDadj
    definition mirrored from eval_windows.dd_adjusted_return — zero-DD legs
    score 0.0 so untraded combos never win); other metrics are read directly.
    """
    if metric == "dd_adjusted_return":
        ret = float(result.get("total_return_pct", 0) or 0)
        dd = float(result.get("max_drawdown_pct", 0) or 0)
        return ret / abs(dd) if dd else 0.0
    v = result.get(metric, 0)
    return v if isinstance(v, (int, float)) else -np.inf


# Default sweep for `--sweep-close` (#996): the audit baseline plus ATR-stop
# and tiered-TP variants around the live default ladder. Ladders are swept as
# whole candidates (a ladder is one grid value, not per-tier dimensions).
_TP_LADDER_DEFAULT = [  # live default (defaultHLProtectionTiers mirror)
    {"atr_multiple": 1.5, "close_fraction": 0.4},
    {"atr_multiple": 3.0, "close_fraction": 0.8},
    {"atr_multiple": 5.0, "close_fraction": 1.0},
]
_TP_LADDER_TIGHT = [  # bank earlier, cut tail risk
    {"atr_multiple": 1.0, "close_fraction": 0.5},
    {"atr_multiple": 2.0, "close_fraction": 0.8},
    {"atr_multiple": 3.0, "close_fraction": 1.0},
]
_TP_LADDER_RUNNER = [  # smaller early clips, let winners run
    {"atr_multiple": 2.0, "close_fraction": 0.33},
    {"atr_multiple": 4.0, "close_fraction": 0.66},
    {"atr_multiple": 6.0, "close_fraction": 1.0},
]

DEFAULT_CLOSE_STACK_SPECS = [
    {"close": None},  # audit baseline: open-signal-as-close, no stops
    {"close": None, "stop_loss_atr_mult": [1.5, 2.0, 3.0]},
    {"close": None, "trailing_stop_atr_mult": [2.0, 2.5, 3.0]},
    {"close": {"name": "tiered_tp_atr",
               "params": {"tp_tiers": [_TP_LADDER_DEFAULT, _TP_LADDER_TIGHT,
                                       _TP_LADDER_RUNNER]}},
     "stop_loss_atr_mult": [None, 1.5, 2.0, 3.0]},
    {"close": {"name": "tiered_tp_atr",
               "params": {"tp_tiers": [_TP_LADDER_DEFAULT, _TP_LADDER_TIGHT,
                                       _TP_LADDER_RUNNER]}},
     "trailing_stop_atr_mult": [2.0, 3.0]},
]


def walk_forward_optimize(
    df: pd.DataFrame,
    strategy_name: str,
    param_ranges: Dict[str, list],
    n_splits: int = 5,
    train_pct: float = 0.7,
    optimize_metric: str = "sharpe_ratio",
    initial_capital: float = 1000.0,
    symbol: str = "BTC/USDT",
    timeframe: str = "1d",
    registry: str = "spot",
    platform: str = "binanceus",
    verbose: bool = True,
    regime_enabled: bool = False,
    regime_period: int = 14,
    regime_adx_threshold: float = 20.0,
    allowed_regimes: Optional[List[str]] = None,
    stop_loss_atr_mult: Optional[float] = None,
    trailing_stop_atr_mult: Optional[float] = None,
    close_strategies: Optional[List[dict]] = None,
    close_stack_grid: Optional[List[dict]] = None,
    direction: Optional[str] = None,
) -> dict:
    """
    Walk-forward optimization.

    1. Split data into n_splits rolling windows
    2. For each window: optimize on train portion, validate on test portion
    3. Report aggregated out-of-sample performance

    Args:
        df: OHLCV DataFrame with datetime index
        strategy_name: Name of registered strategy
        param_ranges: Dict of param_name -> [values to test]
        n_splits: Number of walk-forward windows
        train_pct: Fraction of each window used for training
        optimize_metric: Metric to maximize during optimization
        initial_capital: Starting capital per window
        symbol: Trading pair
        timeframe: Candle timeframe
        close_stack_grid: Optional list of close stacks (#996, from
            ``generate_close_stack_grid``) swept jointly with the open-param
            grid — selection picks the best (open params, close stack) pair
            per fold. Mutually exclusive with the fixed ``close_strategies``/
            stop-mult kwargs.
        direction: Backtester direction gate (long/both; "short" is rejected
            — the walk-forward warmup seeder is long-only, see the guard
            below). When a
            close-stack grid is supplied and direction is omitted it defaults
            to "long": close refs activate the open/close engine where a raw
            signal=-1 OPENS a short, so without the gate the baseline stack
            (plain path, structurally long/flat) and the close-ref stacks
            (long/short) would be scored on different entry universes — the
            sweep comparison must hold the entry side constant.

    Returns:
        Dict with optimization results and best parameters per window
    """
    total_len = len(df)
    window_size = total_len // n_splits
    if window_size < 50:
        raise ValueError(f"Not enough data: {total_len} rows / {n_splits} splits = {window_size} rows per window. Need >= 50.")

    if close_stack_grid and (close_strategies or stop_loss_atr_mult
                             or trailing_stop_atr_mult):
        raise ValueError(
            "close_stack_grid is mutually exclusive with the fixed "
            "close_strategies / stop_loss_atr_mult / trailing_stop_atr_mult "
            "kwargs — the grid owns the close stack")
    if close_stack_grid and direction is None:
        direction = "long"
    # #989 review: walk-forward cannot measure the short leg faithfully yet.
    # The warmup seeder (warmup_exit_long_entry) scans the raw warmup signals
    # for a carried LONG position and feeds it to Backtester.run as
    # starting_long — but under direction="short" a +1 signal never opens
    # anything in the actual run, so the seed would inject a phantom long
    # into the short measurement. Faithful support needs a short-aware
    # warmup seeder; until then reject loudly regardless of close stacks.
    if direction == "short":
        raise ValueError(
            "direction='short' is not supported by walk-forward optimization "
            "yet — the warmup seeder (warmup_exit_long_entry) is long-only "
            "and would carry a phantom long position into the short run. "
            "Measure the short leg with run_backtest.py --mode single "
            "--direction short or eval_windows.py --direction short."
        )

    reg = load_registry(registry)
    apply_strategy = reg.apply_strategy

    param_grid = generate_param_grid(param_ranges)
    warmup = max_indicator_lookback(param_ranges)

    # One Backtester per close stack, constructed up front so invalid stacks
    # fail loudly instead of being silently skipped inside the fold loop.
    if close_stack_grid:
        stacks = close_stack_grid
    else:
        fixed = {
            "close_strategies": list(close_strategies or []),
            "stop_loss_atr_mult": stop_loss_atr_mult,
            "trailing_stop_atr_mult": trailing_stop_atr_mult,
        }
        fixed["label"] = close_stack_label(fixed)
        stacks = [fixed]
    # Guard the whole optimize surface, grid or not: a no-close stack runs on
    # the plain single-leg path, which cannot model "both" (one signal cannot
    # open one side and close the other — Backtester.run rejects it too), so
    # a baseline stack in the grid cannot score the requested universe.
    # direction="short" was rejected wholesale above (warmup seeder).
    if direction == "both" and any(
        not stack.get("close_strategies") for stack in stacks
    ):
        raise ValueError(
            "direction='both' requires a close evaluator on every close "
            "stack — a no-close (baseline) stack runs the plain single-leg "
            "path, which cannot open one side and close the other")
    stack_bts = [
        (stack, Backtester(
            initial_capital=initial_capital, platform=platform,
            regime_enabled=regime_enabled, regime_period=regime_period,
            regime_adx_threshold=regime_adx_threshold,
            allowed_regimes=allowed_regimes,
            stop_loss_atr_mult=stack.get("stop_loss_atr_mult"),
            trailing_stop_atr_mult=stack.get("trailing_stop_atr_mult"),
            close_strategies=stack.get("close_strategies"),
            direction=direction,
        ))
        for stack in stacks
    ]
    bt = stack_bts[0][1]  # slippage reference for warmup_exit_long_entry

    # Any ATR-based exit needs an `atr` column on the signal frame: close
    # evaluators stamp entry_atr from it (mirror of the run_backtest
    # single-mode injection — without it tiered_tp_atr silently no-ops), and
    # the warmup slice needs it so warmup_exit_long_entry can stamp the
    # carried position's entry_atr (bare scalar stops included — the
    # Backtester self-injects ATR for mid-window opens but never sees the
    # warmup bars). ATR(14) also needs warmup bars to prime before the
    # first fill.
    needs_atr = any(stack.get("close_strategies") for stack, _ in stack_bts)
    uses_exits = needs_atr or any(
        stack.get("stop_loss_atr_mult") or stack.get("trailing_stop_atr_mult")
        for stack, _ in stack_bts
    )
    if uses_exits:
        warmup = max(warmup, ATR_CLOSE_WARMUP)

    if verbose:
        print(f"\nWalk-Forward Optimization: {strategy_name}")
        print(f"  Data: {len(df)} candles | Splits: {n_splits} | Train: {train_pct:.0%}")
        print(f"  Parameter combinations: {len(param_grid)}")
        if len(stack_bts) > 1 or close_stack_grid:
            print(f"  Close stacks: {len(stack_bts)} "
                  f"(joint grid: {len(param_grid) * len(stack_bts)} combos)")
        print(f"  Warmup bars per fold: {warmup}")
        print(f"  Optimizing: {optimize_metric}")

    window_results = []

    for fold in range(n_splits):
        start_idx = fold * window_size
        end_idx = min(start_idx + window_size, total_len) if fold < n_splits - 1 else total_len
        window_df = df.iloc[start_idx:end_idx]

        train_size = int(len(window_df) * train_pct)
        train_df = window_df.iloc[:train_size]
        test_df = window_df.iloc[train_size:]

        if len(train_df) < 30 or len(test_df) < 10:
            continue

        # Pre-roll indicator state with ``warmup`` bars of preceding history
        # so long-lookback indicators prime before the first signal bar.
        train_boundary = start_idx + train_size
        train_start_ext = max(0, start_idx - warmup)
        test_start_ext = max(0, train_boundary - warmup)
        train_ext_df = df.iloc[train_start_ext:train_boundary]
        test_ext_df = df.iloc[test_start_ext:end_idx]
        train_trim = start_idx - train_start_ext
        test_trim = train_boundary - test_start_ext

        if verbose:
            print(f"\n  Fold {fold+1}/{n_splits}: "
                  f"Train {train_df.index[0].strftime('%Y-%m-%d')}→{train_df.index[-1].strftime('%Y-%m-%d')} "
                  f"| Test {test_df.index[0].strftime('%Y-%m-%d')}→{test_df.index[-1].strftime('%Y-%m-%d')}")

        # Pass the boundary bar (last warmup row) into the Backtester along
        # with the train rows. Its raw signal then becomes row 1's shifted
        # signal inside Backtester.run, so a BUY/SELL emitted on the final
        # warmup bar fires on the first train bar's open — matching live.
        # The warmup scan runs on bars strictly before the boundary so it
        # doesn't double-count that signal.
        train_boundary_idx = max(train_trim - 1, 0)

        # Optimize on training data — joint (open params x close stack) grid.
        # Signals are computed once per open-param combo and shared across
        # all close stacks (the expensive parts are apply_strategy + bt.run).
        best_metric = -np.inf
        best_params = None
        best_stack = None
        best_bt = None
        for params in param_grid:
            try:
                signals_ext = apply_strategy(strategy_name, train_ext_df, params)
                if uses_exits:
                    signals_ext = ensure_atr_indicator(signals_ext)
                signals_df = signals_ext.iloc[train_boundary_idx:]
                train_seed = warmup_exit_long_entry(
                    signals_ext.iloc[:train_boundary_idx], bt.slippage_pct,
                ) if train_boundary_idx else None
            except _EXPECTED_FOLD_ERRORS as e:
                if verbose:
                    print(f"    [skip] fold {fold+1} {strategy_name} {params}: {type(e).__name__}: {e}")
                continue
            for stack, stack_bt in stack_bts:
                try:
                    result = stack_bt.run(signals_df, strategy_name=strategy_name,
                                          symbol=symbol, timeframe=timeframe,
                                          params=params, save=False,
                                          starting_long=train_seed)
                    metric_val = _result_metric(result, optimize_metric)
                    if metric_val > best_metric:
                        best_metric = metric_val
                        best_params = params
                        best_stack = stack
                        best_bt = stack_bt
                except _EXPECTED_FOLD_ERRORS as e:
                    if verbose:
                        print(f"    [skip] fold {fold+1} {strategy_name} {params} "
                              f"[{stack['label']}]: {type(e).__name__}: {e}")
                    continue

        if best_params is None:
            continue

        # Validate on test data with best params + best close stack
        test_boundary_idx = max(test_trim - 1, 0)
        try:
            test_signals_ext = apply_strategy(strategy_name, test_ext_df, best_params)
            if uses_exits:
                test_signals_ext = ensure_atr_indicator(test_signals_ext)
            test_signals = test_signals_ext.iloc[test_boundary_idx:]
            test_seed = warmup_exit_long_entry(
                test_signals_ext.iloc[:test_boundary_idx], bt.slippage_pct,
            ) if test_boundary_idx else None
            test_result = best_bt.run(test_signals, strategy_name=strategy_name,
                                      symbol=symbol, timeframe=timeframe,
                                      params=best_params, save=False,
                                      starting_long=test_seed)
        except _EXPECTED_FOLD_ERRORS as e:
            if verbose:
                print(f"    [skip] fold {fold+1} validation {strategy_name}: {type(e).__name__}: {e}")
            continue

        window_results.append({
            "fold": fold + 1,
            "best_params": best_params,
            "best_close_stack": best_stack["label"],
            "best_close_stack_spec": {
                k: best_stack.get(k) for k in
                ("close_strategies", "stop_loss_atr_mult", "trailing_stop_atr_mult")
            },
            "train_metric": best_metric,
            "test_result": test_result,
            "train_period": f"{train_df.index[0].strftime('%Y-%m-%d')} to {train_df.index[-1].strftime('%Y-%m-%d')}",
            "test_period": f"{test_df.index[0].strftime('%Y-%m-%d')} to {test_df.index[-1].strftime('%Y-%m-%d')}",
        })

        if verbose:
            print(f"    Best params: {best_params}")
            if len(stack_bts) > 1:
                print(f"    Best close stack: {best_stack['label']}")
            print(f"    Train {optimize_metric}: {best_metric:.3f}")
            print(f"    Test return: {test_result['total_return_pct']:+.2f}% | "
                  f"Sharpe: {test_result['sharpe_ratio']:.3f} | "
                  f"MaxDD: {test_result['max_drawdown_pct']:.2f}%")

    if not window_results:
        return {"error": "No valid optimization windows", "strategy": strategy_name}

    # Aggregate out-of-sample results
    oos_returns = [w["test_result"]["total_return_pct"] for w in window_results]
    oos_sharpes = [w["test_result"]["sharpe_ratio"] for w in window_results]
    oos_drawdowns = [w["test_result"]["max_drawdown_pct"] for w in window_results]

    # Find most common best params / close stack
    all_params = [str(w["best_params"]) for w in window_results]
    from collections import Counter
    most_common_params = Counter(all_params).most_common(1)[0][0]
    most_common_stack = Counter(
        w["best_close_stack"] for w in window_results
    ).most_common(1)[0][0]

    summary = {
        "strategy": strategy_name,
        "n_splits": n_splits,
        "n_valid_folds": len(window_results),
        "param_grid_size": len(param_grid),
        "close_stack_grid_size": len(stack_bts),
        "optimize_metric": optimize_metric,
        "oos_mean_return": round(np.mean(oos_returns), 2),
        "oos_median_return": round(np.median(oos_returns), 2),
        "oos_std_return": round(np.std(oos_returns), 2),
        "oos_mean_sharpe": round(np.mean(oos_sharpes), 3),
        "oos_mean_drawdown": round(np.mean(oos_drawdowns), 2),
        "oos_worst_drawdown": round(min(oos_drawdowns), 2),
        "most_common_best_params": most_common_params,
        "most_common_best_close_stack": most_common_stack,
        "window_results": window_results,
    }

    if verbose:
        print(f"\n{'='*60}")
        print(f"  WALK-FORWARD SUMMARY: {strategy_name}")
        print(f"{'='*60}")
        print(f"  Valid folds: {summary['n_valid_folds']}/{n_splits}")
        print(f"  OOS Mean Return: {summary['oos_mean_return']:+.2f}%")
        print(f"  OOS Median Return: {summary['oos_median_return']:+.2f}%")
        print(f"  OOS Return StdDev: {summary['oos_std_return']:.2f}%")
        print(f"  OOS Mean Sharpe: {summary['oos_mean_sharpe']:.3f}")
        print(f"  OOS Mean MaxDD: {summary['oos_mean_drawdown']:.2f}%")
        print(f"  OOS Worst MaxDD: {summary['oos_worst_drawdown']:.2f}%")
        print(f"  Most Stable Params: {summary['most_common_best_params']}")
        if len(stack_bts) > 1:
            print(f"  Most Stable Close Stack: {summary['most_common_best_close_stack']}")

    return summary


# Predefined parameter ranges for optimization.
# Grids are kept small (≤ ~32 combinations) to bound walk-forward runtime;
# each strategy registered in shared_strategies/{spot,futures}/strategies.py
# should have an entry here. Missing entries fall back to the strategy's
# default_params (single-point grid) so optimize mode still runs.
DEFAULT_PARAM_RANGES = {
    "sma_crossover": {
        "fast_period": [10, 15, 20, 25],
        "slow_period": [40, 50, 60, 80],
    },
    "ema_crossover": {
        "fast_period": [8, 12, 16, 20],
        "slow_period": [21, 26, 34, 50],
    },
    "rsi": {
        "period": [10, 14, 21],
        "overbought": [65, 70, 75],
        "oversold": [25, 30, 35],
    },
    "bollinger_bands": {
        "period": [15, 20, 25, 30],
        "num_std": [1.5, 2.0, 2.5],
    },
    "macd": {
        "fast_period": [8, 12, 16],
        "slow_period": [21, 26, 34],
        "signal_period": [7, 9, 12],
    },
    "mean_reversion": {
        "lookback": [20, 30, 40, 50],
        "entry_std": [1.0, 1.5, 2.0],
        "exit_std": [0.0, 0.5, 1.0],
    },
    "momentum": {
        "roc_period": [10, 14, 21, 30],
        "threshold": [3.0, 5.0, 7.0, 10.0],
    },
    "volume_weighted": {
        "sma_period": [15, 20, 30],
        "vol_multiplier": [1.2, 1.5, 2.0],
    },
    "triple_ema": {
        "short_period": [5, 8, 13],
        "mid_period": [15, 21, 30],
        "long_period": [40, 55, 80],
    },
    "triple_ema_bidir": {
        "short_period": [5, 8, 13],
        "mid_period": [15, 21, 30],
        "long_period": [40, 55, 80],
    },
    "tema_cross": {
        "short_period": [3, 5, 8],
        "mid_period": [10, 13, 18],
        "long_period": [25, 34, 50],
    },
    "tema_cross_bd": {
        "short_period": [3, 5, 8],
        "mid_period": [10, 13, 18],
        "long_period": [25, 34, 50],
    },
    "rsi_macd_combo": {
        "rsi_period": [10, 14],
        "rsi_oversold": [30, 35, 40],
        "rsi_overbought": [60, 65, 70],
        "macd_fast": [8, 12],
        "macd_slow": [21, 26],
        "macd_signal": [7, 9],
    },
    "stoch_rsi": {
        "rsi_period": [10, 14, 21],
        "stoch_period": [10, 14, 21],
        "overbought": [75, 80, 85],
        "oversold": [15, 20, 25],
    },
    "supertrend": {
        "atr_period": [7, 10, 14],
        "multiplier": [2.0, 3.0, 4.0],
    },
    "ichimoku_cloud": {
        "tenkan_period": [7, 9, 11],
        "kijun_period": [22, 26, 30],
        "senkou_b_period": [44, 52, 60],
    },
    "pairs_spread": {
        "lookback": [20, 30, 40, 50],
        "entry_z": [1.5, 2.0, 2.5],
        "exit_z": [0.0, 0.5, 1.0],
    },
    "squeeze_momentum": {
        "bb_period": [15, 20, 25],
        "kc_period": [15, 20, 25],
        "kc_mult": [1.0, 1.5, 2.0],
        "mom_lookback": [8, 12, 16],
    },
    "atr_breakout": {
        "atr_period": [10, 14, 20],
        "multiplier": [1.0, 1.5, 2.0],
    },
    "amd_ifvg": {
        "min_ifvg_pct": [0.03, 0.05, 0.1],
        "sweep_threshold_pct": [0.005, 0.01, 0.02],
    },
    "heikin_ashi_ema": {
        "ema_period": [13, 21, 34],
        "confirmation": [1, 2, 3],
    },
    "order_blocks": {
        "atr_period": [10, 14, 20],
        "displacement_mult": [1.0, 1.5, 2.0],
        "ob_lookback": [15, 20, 30],
        "max_ob_age": [30, 50, 80],
    },
    "vwap_reversion": {
        "entry_std": [1.0, 1.5, 2.0],
        "exit_std": [0.0, 0.2, 0.5],
    },
    "anchored_vwap": {
        "pivot_strength": [3, 5, 8],
        "buffer_atr_mult": [0.1, 0.25, 0.5],
        "confirm_bars": [1, 2, 3],
    },
    "chart_pattern": {
        "pivot_lookback": [3, 5, 7],
        "tolerance": [0.02, 0.03, 0.05],
        "vol_multiplier": [1.2, 1.5, 2.0],
    },
    "liquidity_sweeps": {
        "swing_lookback": [10, 20, 30],
        "confirmation": [1, 2, 3],
    },
    "parabolic_sar": {
        "iaf": [0.01, 0.02, 0.03],
        "af_step": [0.01, 0.02, 0.03],
        "max_af": [0.1, 0.2, 0.3],
    },
    "range_scalper": {
        "bb_period": [10, 14, 20],
        "bw_threshold": [0.005, 0.008, 0.012],
        "rsi_period": [5, 7, 10],
    },
    "sweep_squeeze_combo": {
        "swing_lookback": [5, 10, 15],
        "min_agree": [2, 3],
    },
    "adx_trend": {
        "adx_period": [10, 14, 20],
        "adx_threshold": [20, 25, 30],
    },
    "donchian_breakout": {
        "entry_period": [10, 20, 30],
        "exit_period": [5, 10, 15],
    },
    "momentum_pro": {
        "ema_fast": [13, 20, 26],
        "ema_mid": [34, 50, 80],
        # Single value (243 combos). Present so max_indicator_lookback provisions
        # 200 warmup bars per fold and the EMA(200) regime gate is primed OOS.
        "ema_long": [200],
        "adx_threshold": [18.0, 20.0, 25.0],
        "pullback_window": [4, 6, 8],
        "vol_mult": [0.0, 1.2, 1.5],
    },
    "mean_reversion_pro": {
        "lookback": [20, 30, 40],
        "entry_std": [1.5, 2.0, 2.5],
        "adx_max": [20.0, 25.0, 30.0],
        "rsi_oversold": [25.0, 30.0, 35.0],
        "rsi_overbought": [65.0, 70.0, 75.0],
    },
    "mtf_confluence": {
        "htf_factor": [3, 4, 6],
        "htf_ema_fast": [10, 20, 26],
        # NOTE: the real warmup need is ~htf_factor × htf_ema_slow NATIVE bars
        # (the slow EMA counts HTF buckets), which max_indicator_lookback's
        # raw-int heuristic can't see. Safe regardless: the strategy holds the
        # HTF trend neutral until primed, so under-provisioned folds just
        # trade less at the start — never on look-ahead or junk EMAs.
        "htf_ema_slow": [40, 50],
        "ltf_ema": [13, 20],
        "pullback_window": [4, 6, 8],
    },
    "vol_momentum": {
        # mom_window dominates warmup; atr_period (14) is below every value here
        # so max_indicator_lookback provisions enough history per fold.
        "mom_window": [16, 24, 48],
        "entry_threshold": [0.20, 0.30, 0.40],
        "exit_threshold": [0.0, 0.05, 0.10],
        "eff_entry": [0.25, 0.35, 0.45],
    },
    "funding_skew": {
        "funding_window": [96, 168, 336],
        "z_entry": [1.0, 1.5, 2.0],
        "z_exit": [0.25, 0.5],
        "confirm_ema": [10, 20, 40],
    },
    "regime_adaptive": {
        "period": [14, 20, 30],
        "adx_threshold": [20.0, 25.0, 30.0],
        "efficiency_threshold": [0.3, 0.4, 0.5],
        "breakout_lookback": [10, 20, 30],
        "mr_entry_z": [1.5, 2.0],
        "slow_veto_threshold": [0.03, 0.05, 0.08],
    },
    "regime_adaptive_htf": {
        # Real warmup is ~htf_factor × (period + confirm_buckets) NATIVE bars
        # (composite metrics count HTF buckets), which max_indicator_lookback's
        # raw-int heuristic can't see; the pinned slow_trend_lookback value
        # provisions 100 bars and under-provisioned folds stay flat until the
        # label primes — never look-ahead (same caveat as mtf_confluence).
        "htf_factor": [4, 6, 8],
        "confirm_buckets": [1, 2, 3],
        "period": [10, 14],
        "mr_entry_z": [1.75, 2.0, 2.25],
        # Drift-confirmed trend participation: "breakout" dominates in
        # trending years (2023/2024), "off" in grind years (2026 OOS) — see
        # the regime_adaptive_htf module docstring's per-window table.
        "trend_entry": ["off", "breakout"],
        "slow_trend_lookback": [100],
    },
    "consolidation_range": {
        "box_width_pct": [0.03, 0.05, 0.08, 0.10],
        "min_bars": [12, 16, 24],
        "edge_entry_frac": [0.1, 0.2, 0.33],
    },
    "bear_pullback_st": {
        "ema_short": [13, 20, 26],
        "ema_mid": [34, 50, 80],
        "adx_threshold": [18, 20, 25],
        "pullback_window": [3, 5, 8],
    },
    "vwap_rejection_st": {
        "ema_short": [13, 20, 26],
        "ema_mid": [34, 50, 80],
        "rsi_max_reclaim": [45.0, 50.0, 55.0],
        "rally_window": [3, 5, 8],
        "rally_touch_buffer_pct": [0.0005, 0.001, 0.002],
    },
    # NOTE: close-evaluator names (tp_at_pct, tiered_tp_pct, tiered_tp_atr, …) are
    # intentionally absent here. walk_forward_optimize only sweeps OPEN-registry
    # strategy params through THIS table (it builds the entry signal via the open
    # registry), so a close-param grid keyed by a close name would be dead weight
    # — unreachable and never swept. Close-param optimization lives in the
    # close-stack grid instead (#996): DEFAULT_CLOSE_STACK_SPECS /
    # generate_close_stack_grid, swept jointly via the close_stack_grid kwarg.
    # Do not re-add close names here (#944).
    # Futures-only
    "breakout": {
        "lookback": [10, 20, 30],
        "atr_period": [10, 14, 20],
        "atr_multiplier": [1.0, 1.5, 2.0],
    },
    "session_breakout": {
        "session": ["asian", "us_open", "us_close"],
        "lookback": [1, 2, 3],
        "volume_threshold": [1.2, 1.5, 2.0],
        "atr_multiplier": [0.0, 0.5, 1.0],
    },
    "delta_neutral_funding": {
        "entry_threshold": [0.00005, 0.0001, 0.00015],
        "exit_threshold": [0.0, 0.00002, 0.00005],
        "drift_threshold": [1.5, 2.0, 2.5],
    },
    "hold": {},  # no parameters — always signal=0; used by type=manual close-eval loop (#569)
}


if __name__ == "__main__":
    # Test with synthetic data
    np.random.seed(42)
    dates = pd.date_range("2020-01-01", periods=500, freq="D")
    prices = 100 + np.cumsum(np.random.randn(500) * 2)
    df = pd.DataFrame({
        "open": prices,
        "high": prices + abs(np.random.randn(500)),
        "low": prices - abs(np.random.randn(500)),
        "close": prices + np.random.randn(500) * 0.5,
        "volume": np.random.randint(1000, 10000, 500).astype(float),
    }, index=dates)

    result = walk_forward_optimize(
        df, "sma_crossover",
        {"fast_period": [10, 20], "slow_period": [40, 50]},
        n_splits=3, verbose=True
    )

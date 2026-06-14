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

# Strategies whose signals read a per-bar `funding_rate` column. The column is
# attached after the OHLCV load (Hyperliquid hourly funding, cached in SQLite,
# merge_asof backward — see shared_tools/funding_fetcher). Without it these
# strategies are fail-safe flat, which silently zeroes the backtest, so the
# attach failure warns loudly.
FUNDING_COLUMN_STRATEGIES = {"funding_skew", "delta_neutral_funding"}

# Strategies whose PnL is carry, not price direction (#988): they additionally
# get a `funding_accrual` column (total funding over each bar's interval), which
# the Backtester books against the held position. Without booking the carry, a
# delta-neutral funding strategy's result is meaningless (it shorts to collect
# funding, so its edge lives entirely in the accrual the engine would otherwise
# ignore). funding_skew is intentionally excluded — its edge is the price move
# and its M1 baseline was established without accrual; adding it here would move
# that baseline. See the issue for the (separate) follow-up.
FUNDING_ACCRUAL_STRATEGIES = {"delta_neutral_funding"}


def _attach_funding_if_needed(df, strategy_name, symbol, since):
    if strategy_name not in FUNDING_COLUMN_STRATEGIES or df.empty:
        return df
    from funding_fetcher import (attach_funding_accrual_column,
                                 attach_funding_column, load_cached_funding)
    coin = symbol.split("/")[0]
    try:
        # Pass the data's actual window end so a repeat run over the same
        # window is a cache hit regardless of elapsed wall-clock time
        # (end_date=None compares coverage against `now`, refetching forever).
        funding = load_cached_funding(coin, since, end_date=df.index[-1])
    except Exception as e:
        print(f"[WARN] funding history fetch failed for {coin}: {e} — "
              f"'{strategy_name}' will produce zero entries.")
        funding = None
    out = attach_funding_column(df, funding)
    have = int(out["funding_rate"].notna().sum())
    if have == 0:
        print(f"[WARN] no funding data attached for {coin} since {since} — "
              f"'{strategy_name}' will produce zero entries.")
    else:
        print(f"  Funding: {have}/{len(out)} bars covered (HL hourly, coin={coin})")
    if strategy_name in FUNDING_ACCRUAL_STRATEGIES:
        # Carry to book against the held position (timeframe-correct sum, not
        # the signal snapshot). The engine auto-detects the column.
        out = attach_funding_accrual_column(out, funding)
    return out
from htf_filter import get_default_htf, apply_htf_filter  # noqa: E402
from registry_loader import load_registry
from backtester import Backtester, format_results
from optimizer import (walk_forward_optimize, DEFAULT_PARAM_RANGES,
                       DEFAULT_CLOSE_STACK_SPECS, generate_close_stack_grid)
from reporter import (
    format_single_report, format_comparison_report,
    format_multi_asset_report, format_walk_forward_report,
    generate_full_report,
)
from regime import compute_regime, compute_regime_composite  # noqa: E402


def _normalize_regime_window_spec(spec) -> dict:
    """Normalize a regime.windows entry into a {classifier, period, ...} dict.
    A bare int is ADX shorthand (mirrors Go RegimeWindowsMap.UnmarshalJSON)."""
    if isinstance(spec, (int, float)) and not isinstance(spec, bool):
        return {"classifier": "adx", "period": int(spec)}
    spec = dict(spec or {})
    classifier = str(spec.get("classifier") or "adx").strip().lower() or "adx"
    out = {"classifier": classifier, "period": int(spec.get("period") or 0)}
    if classifier == "adx":
        out["adx_threshold"] = float(spec.get("adx_threshold") or 20.0)
    else:
        out["thresholds"] = dict(spec.get("thresholds") or {})
    return out


def _build_profile_label_series(df: pd.DataFrame, window_spec: dict) -> pd.Series:
    """Compute the per-bar regime label at the switch window's classifier/period
    (#998). Mirrors the live regime store: composite via compute_regime_composite,
    ADX via compute_regime. The result is the bar-close label; Backtester shifts
    it by one so bar N's label governs the N+1 fill (look-ahead guard)."""
    classifier = window_spec.get("classifier", "adx")
    period = int(window_spec.get("period") or 14)
    if classifier == "composite":
        reg = compute_regime_composite(df, period=period,
                                       thresholds=window_spec.get("thresholds") or None)
    else:
        reg = compute_regime(df, period=period,
                             adx_threshold=float(window_spec.get("adx_threshold") or 20.0))
    return reg["regime"].astype(str)


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


# #866: close evaluators whose default ladder is overridable via
# user_close_defaults. Mirrors scheduler/close_defaults.go closeDefaultsSupported
# — the evaluators that resolve purely through tp_tiers (the regime tiered-ATR
# variants are excluded; their use_defaults baseline is #870 territory).
_USER_CLOSE_DEFAULTS_SUPPORTED = {
    "tiered_tp_pct",
    "tiered_tp_atr",
    "tiered_tp_atr_live",
    "trailing_tp_ratchet",
    "trailing_tp_ratchet_regime",
}


def _apply_user_close_defaults(close_refs: list, user_defaults: Optional[dict]) -> None:
    """Inject user_close_defaults tp_tiers into close refs that omit tp_tiers
    (#866, --defaults user). Mirrors the Go injection at load: an explicit
    per-ref tp_tiers (the strategy layer) wins, an unsupported evaluator name is
    skipped, and a ref with no matching entry falls through to the evaluator's
    built-in system default."""
    if not user_defaults:
        return
    for ref in close_refs:
        name = str(ref.get("name", "")).strip().lower()
        if name not in _USER_CLOSE_DEFAULTS_SUPPORTED:
            continue
        params = ref.setdefault("params", {})
        if params.get("tp_tiers") is not None:
            continue  # strategy_close_defaults layer wins
        entry = user_defaults.get(name)
        if not isinstance(entry, dict):
            continue
        tp = entry.get("tp_tiers")
        # Mirror the Go loader (validateUserCloseDefaults): an empty or
        # wrong-typed tp_tiers is not a valid override — injecting [] would
        # suppress the system default. Skip it so resolution falls through to the
        # evaluator's built-in default, matching the daemon (which rejects such a
        # config outright at load).
        if (isinstance(tp, list) or isinstance(tp, dict)) and tp:
            params["tp_tiers"] = tp


def _effective_direction(sc: dict) -> str:
    """Resolve a strategy entry's effective entry direction (#942).

    Mirrors ``EffectiveDirection`` (scheduler/config.go): direction is
    meaningful only for ``perps``/``manual`` (other types are long by
    construction). An explicit ``"long"``/``"short"``/``"both"`` wins; otherwise
    the legacy ``allow_shorts`` toggle maps absent->``"long"`` / true->``"both"``
    so pre-v14 configs gate identically to live.
    """
    if str(sc.get("type") or "perps") not in ("perps", "manual"):
        return "long"
    d = str(sc.get("direction") or "").strip().lower()
    if d in ("long", "short", "both"):
        return d
    return "both" if sc.get("allow_shorts") else "long"


def load_strategy_config(config_path: str, strategy_id: str,
                         inject_user_defaults: bool = False) -> dict:
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
    # #942 (D2.8): gate on v15, not v13. The v13 co-located ref shape (#640) is
    # necessary but not sufficient — the v15 migration is what canonicalizes
    # close-strategy params on disk (tiers->tp_tiers, atr/multiple/fraction->
    # atr_multiple/close_fraction; config_migration_v15.go). The Python close
    # evaluators read ONLY the canonical runtime keys (tier_list_from_params
    # reads tp_tiers exclusively), so a pre-v15 config passes the old gate while
    # its legacy close keys silently no-op: explicit tiers are dropped to the
    # system-default ladder, and --defaults user injects user defaults over the
    # operator's legacy tiers. Live is unaffected (Go canonicalizes on read), so
    # backtest and live would silently diverge on the same file. Reject instead.
    if version < 15:
        raise ValueError(
            f"{config_path}: config_version={version} predates the v15 "
            f"close-param canonicalization (tiers->tp_tiers, atr/multiple/"
            f"fraction->atr_multiple/close_fraction). The backtest close "
            f"evaluators read only the canonical runtime keys, so a pre-v15 "
            f"config's legacy close params would silently no-op (diverging from "
            f"live, which canonicalizes on read). Run go-trader once against "
            f"this file to migrate it, then retry."
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
        # #779: regime_directional_policy is HL-live-only — the backtester
        # has no per-cycle resolver hook that mirrors runHyperliquidCheck,
        # so the per-regime Direction / InvertSignal flip wouldn't apply
        # and results would silently use the static config instead. Reject
        # at config-load time so the operator sees the gap (parity with
        # the regime-aware `sl_after` rejection at backtester init).
        if sc.get("regime_directional_policy"):
            raise ValueError(
                f"{config_path}: strategy {strategy_id!r} uses "
                f"regime_directional_policy, which is HL-live-only in this "
                f"release (backtester parity deferred — see #779). Use the "
                f"static `direction` / `invert_signal` fields for backtesting."
            )
        # #942 (D2.5): regime_window_divergence (#907) is HL-perps-live-only —
        # it mutates sc.Direction per-cycle off a live short/medium regime
        # split, which the bar-level backtester has no resolver hook to mirror.
        # Unlike its rejected siblings (regime_directional_policy above,
        # tiered_tp_atr_live_regime_dynamic below) it was silently ignored
        # rather than rejected, so a divergence-driven flip would backtest the
        # static config and diverge from live. Reject with the same wording
        # pattern so the operator sees the gap.
        if sc.get("regime_window_divergence"):
            raise ValueError(
                f"{config_path}: strategy {strategy_id!r} uses "
                f"regime_window_divergence, which is HL-live-only in this "
                f"release (backtester parity deferred — see #907). Use the "
                f"static `direction` / `invert_signal` fields for backtesting."
            )
        # #842: a strategy has a single close_strategy ref. Still accept the
        # legacy close_strategies array (length <=1 after the collapse) so old
        # configs keep backtesting; the backtester's close_strategies= list
        # interface is fed the 0-or-1 element list.
        close_refs = []
        single = sc.get("close_strategy")
        if isinstance(single, dict) and single.get("name"):
            close_refs.append({"name": single["name"], "params": dict(single.get("params") or {})})
        else:
            legacy = sc.get("close_strategies", []) or []
            # Match the live Go loader: the array model collapsed to a single
            # close_strategy (#842). A len>1 legacy array would run here under the
            # old max-fraction semantics while the scheduler rejects it at load —
            # reject the same way so backtest and live can't silently diverge.
            if len(legacy) > 1:
                raise ValueError(
                    f"{config_path}: strategy {strategy_id!r} has "
                    f"{len(legacy)} close_strategies; the array model was "
                    f"collapsed to a single close_strategy (#842). Keep one "
                    f"profit-taking close and move risk backstops to "
                    f"strategy-level stop fields."
                )
            for ref in legacy:
                if isinstance(ref, dict) and ref.get("name"):
                    close_refs.append({"name": ref["name"], "params": dict(ref.get("params") or {})})
        for ref in close_refs:
            if ref.get("name") == "tiered_tp_atr_live_regime_dynamic":
                raise ValueError(
                    f"{config_path}: strategy {strategy_id!r} uses "
                    f"tiered_tp_atr_live_regime_dynamic, which is HL-live-only "
                    f"in this release (backtester parity deferred — see #843)."
                )
        # #866: with --defaults user, apply the config's user_close_defaults to
        # any close ref that omits tp_tiers (so backtest matches the live daemon
        # under the operator override). --defaults system leaves them untouched,
        # falling through to the evaluators' built-in defaults.
        if inject_user_defaults:
            _apply_user_close_defaults(close_refs, cfg.get("user_close_defaults"))
        # #942 (D2.1): model the live entry transforms the backtester used to
        # drop silently. ``invert_signal`` flips BUY<->SELL; ``direction`` gates
        # which side may open. Both are applied to the signal in Backtester.run
        # (see _apply_direction_invert), in the live order (invert then gate).
        direction = _effective_direction(sc)
        invert_signal = bool(sc.get("invert_signal"))
        strategy_type = str(sc.get("type") or "perps")
        # #942 review: invert_signal is HL-perps/manual-only. LoadConfig
        # (config.go) REJECTS the config at startup for any other type/platform
        # because runHyperliquidCheck (applySignalInversion) is its only
        # consumer — spot/options/futures check scripts emit their own buy/sell
        # logic that the Go invert never sees. _effective_direction already
        # forces non-perps types to long-by-construction; without the matching
        # invert gate a stray invert_signal on a spot/futures --config would
        # silently flip BUY<->SELL in the backtest (and then mask the inverted
        # short), producing numbers for a config the live daemon would refuse to
        # load. Reject it the same way, with the established loud-rejection
        # pattern, instead of diverging silently.
        if invert_signal and strategy_type not in ("perps", "manual"):
            raise ValueError(
                f"{config_path}: strategy {strategy_id!r} sets invert_signal "
                f"on type={strategy_type!r}, but invert_signal is HL-perps/"
                f"manual-only (the live daemon rejects this config at startup — "
                f"config.go). Remove invert_signal or backtest a perps/manual "
                f"strategy."
            )
        # The plain signal path (no close evaluator) is structurally
        # single-leg: long/flat by default, short/flat under direction="short"
        # (#989 — signal=-1 opens the short, +1 closes it). direction="both"
        # remains unmodelable there (one signal cannot open one side and close
        # the other), so it would silently backtest long-only — the exact
        # silent-divergence class this parity fix closes. Require the
        # open/close engine path (a close evaluator, which models both open
        # sides) or reject loudly.
        if not close_refs and direction == "both":
            raise ValueError(
                f"{config_path}: strategy {strategy_id!r} has "
                f"direction='both' but no close_strategy. The backtester's "
                f"plain signal path runs one leg at a time (long/flat, or "
                f"short/flat under direction='short'), so the short side of a "
                f"'both' config would be silently dropped. Add a "
                f"close_strategy (the open/close engine models both sides) or "
                f"backtest each leg separately."
            )
        # #998: regime_profile_allocation is backtestable (unlike its rejected
        # siblings) — the slow long-window switch is a pure function of closed-bar
        # OHLCV, so Backtester replays it. Resolve the referenced switch window's
        # spec from the config's regime.windows so run_single_backtest can compute
        # the per-bar label series; reject loudly if the window is absent.
        profile_allocation = None
        pal = sc.get("regime_profile_allocation")
        if pal:
            window = str(pal.get("window") or "").strip()
            regime_cfg = cfg.get("regime") or {}
            windows = regime_cfg.get("windows") or {}
            spec = windows.get(window)
            if not regime_cfg.get("enabled"):
                raise ValueError(
                    f"{config_path}: strategy {strategy_id!r} uses "
                    f"regime_profile_allocation but regime.enabled is not true."
                )
            if spec is None:
                raise ValueError(
                    f"{config_path}: regime_profile_allocation.window={window!r} "
                    f"not found in regime.windows (have: {sorted(windows)})."
                )
            profile_allocation = {
                "window": window,
                "window_spec": _normalize_regime_window_spec(spec),
                "profiles": dict(pal.get("profiles") or {}),
                "param_sets": {
                    k: dict(v or {}) for k, v in (pal.get("param_sets") or {}).items()
                },
                "confirm_bars": int(pal.get("confirm_bars") or 0),
                "initial_profile": str(pal.get("initial_profile") or "").strip(),
            }
        return {
            "open_strategy": {
                "name": open_ref["name"],
                "params": dict(open_ref.get("params") or {}),
            },
            "close_strategies": close_refs,
            "stop_loss_atr_mult": sc.get("stop_loss_atr_mult"),
            "stop_loss_pct": sc.get("stop_loss_pct"),
            "stop_loss_margin_pct": sc.get("stop_loss_margin_pct"),
            "trailing_stop_atr_mult": sc.get("trailing_stop_atr_mult"),
            "trailing_stop_pct": sc.get("trailing_stop_pct"),
            "stop_loss_atr_regime": sc.get("stop_loss_atr_regime"),
            "trailing_stop_atr_regime": sc.get("trailing_stop_atr_regime"),
            "strategy_type": strategy_type,
            "direction": direction,
            "invert_signal": invert_signal,
            "profile_allocation": profile_allocation,
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
    stop_loss_atr_mult: Optional[float] = None,
    stop_loss_pct: Optional[float] = None,
    stop_loss_margin_pct: Optional[float] = None,
    trailing_stop_atr_mult: Optional[float] = None,
    trailing_stop_pct: Optional[float] = None,
    stop_loss_atr_regime: Optional[dict] = None,
    trailing_stop_atr_regime: Optional[dict] = None,
    strategy_type: str = "perps",
    direction: Optional[str] = None,
    invert_signal: bool = False,
    profile_allocation: Optional[dict] = None,
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
    df = _attach_funding_if_needed(df, strategy_name, symbol, since)

    print(f"  Data: {len(df)} candles from {df.index[0]} to {df.index[-1]}")

    if profile_allocation:
        # #998: compute one signal series per profile (base params overlaid with
        # the profile's param_set) plus the long-window label series, then hand
        # the multi-profile frame to the engine, which replays the switch.
        param_sets = profile_allocation["param_sets"]
        names = sorted(param_sets)
        df_signals = None
        for p in names:
            p_params = {**(strat_params or {}), **(param_sets[p] or {})}
            res = reg.apply_strategy(strategy_name, df, p_params)
            if df_signals is None:
                # Seed from the first profile's full frame (OHLCV + indicators)
                # and rename its signal; later profiles contribute only signals.
                df_signals = res.copy()
                df_signals["signal__" + p] = df_signals.pop("signal")
            else:
                df_signals["signal__" + p] = res["signal"].values
        if close_strategies:
            df_signals = ensure_atr_indicator(df_signals)
        df_signals["_profile_label"] = _build_profile_label_series(
            df_signals, profile_allocation["window_spec"]
        ).values
        print(f"  Profile allocation: window={profile_allocation['window']} "
              f"profiles={names} confirm_bars={profile_allocation['confirm_bars']}")
    else:
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
        stop_loss_atr_mult=stop_loss_atr_mult,
        stop_loss_pct=stop_loss_pct,
        stop_loss_margin_pct=stop_loss_margin_pct,
        trailing_stop_atr_mult=trailing_stop_atr_mult,
        trailing_stop_pct=trailing_stop_pct,
        stop_loss_atr_regime=stop_loss_atr_regime,
        trailing_stop_atr_regime=trailing_stop_atr_regime,
        strategy_type=strategy_type,
        direction=direction,
        invert_signal=invert_signal,
        profile_allocation=profile_allocation,
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
    direction: Optional[str] = None,
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
            direction=direction,
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
    direction: Optional[str] = None,
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
                direction=direction,
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
    stop_loss_atr_mult: Optional[float] = None,
    trailing_stop_atr_mult: Optional[float] = None,
    close_strategies: Optional[List[dict]] = None,
    close_stack_grid: Optional[List[dict]] = None,
    optimize_metric: str = "sharpe_ratio",
    direction: Optional[str] = None,
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
    df = _attach_funding_if_needed(df, strategy_name, symbol, since)

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
        stop_loss_atr_mult=stop_loss_atr_mult,
        trailing_stop_atr_mult=trailing_stop_atr_mult,
        close_strategies=close_strategies,
        close_stack_grid=close_stack_grid,
        optimize_metric=optimize_metric,
        direction=direction,
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
    parser.add_argument("--defaults", choices=["system", "user"], default="system",
                        help="Which close-default layer to apply when a close ref omits tp_tiers (#866): "
                             "'system' (default) uses the built-in defaults; 'user' applies the "
                             "user_close_defaults block from --config. Per-strategy tp_tiers always wins. "
                             "Requires --config when set to 'user'.")
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
    parser.add_argument("--stop-loss-atr-mult", type=float, default=None,
                        dest="stop_loss_atr_mult", metavar="MULT",
                        help="Fixed ATR-multiple stop loss (e.g. 2.0). Applied in "
                             "single and optimize/walk-forward modes.")
    parser.add_argument("--trailing-stop-atr-mult", type=float, default=None,
                        dest="trailing_stop_atr_mult", metavar="MULT",
                        help="Trailing ATR-multiple stop (e.g. 2.5). Applied in "
                             "optimize/walk-forward mode.")
    parser.add_argument("--sweep-close", action="store_true",
                        help="Optimize mode (#996): sweep the built-in close-stack "
                             "grid (DEFAULT_CLOSE_STACK_SPECS — baseline, ATR "
                             "stops, tiered-TP ladders) jointly with the open-"
                             "param grid.")
    parser.add_argument("--close-stacks-json", default=None, metavar="PATH",
                        help="Optimize mode (#996): JSON file with a list of "
                             "close-stack sweep specs (see optimizer."
                             "generate_close_stack_grid) swept jointly with "
                             "the open-param grid. Overrides --sweep-close.")
    parser.add_argument("--optimize-metric", default="sharpe_ratio",
                        choices=["sharpe_ratio", "total_return_pct",
                                 "dd_adjusted_return"],
                        help="Selection metric for optimize mode (default: "
                             "sharpe_ratio). dd_adjusted_return = return / "
                             "|max DD| (#963 DDadj).")
    parser.add_argument("--direction", default=None,
                        choices=["long", "short", "both"],
                        help="Side the engine may OPEN; forwarded to every "
                             "mode (#989). 'short' on the plain single-leg "
                             "path runs the short/flat mirror (signal=-1 "
                             "opens, +1 closes); 'both' requires a close "
                             "evaluator. In optimize mode defaults to long "
                             "when a close-stack grid is swept so every "
                             "stack scores on the same entry universe.")
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

    # #866: --defaults user only has an effect via the config's user_close_defaults.
    if args.defaults == "user" and not args.config:
        print("--defaults user requires --config (user_close_defaults lives in the config); "
              "falling back to system defaults")

    # #641: --config loads a single strategy by ID and uses its refs directly.
    open_params: Optional[dict] = None
    live_stop_kwargs: dict = {}
    if args.config:
        # --config loads exactly one strategy; non-single modes would silently
        # ignore the loaded refs for every strategy except the one matching
        # --strategy. Reject upfront instead of producing misleading reports.
        if args.mode != "single":
            print("--config is only valid with --mode single (loads one strategy by --strategy <id>)")
            sys.exit(1)
        live_kwargs = load_strategy_config(args.config, args.strategy,
                                           inject_user_defaults=(args.defaults == "user"))
        # Live config refs take precedence; --close-strategy on top is rejected
        # to avoid silent overrides.
        if close_refs:
            print("--close-strategy is not allowed alongside --config (refs come from the live config)")
            sys.exit(1)
        # #989 review: the live config's `direction` field owns the entry
        # transform; a CLI --direction losing to it via setdefault would
        # silently score the wrong leg — the exact divergence class the flag
        # exists to prevent. Reject loudly, like --close-strategy above.
        if args.direction:
            print("--direction is not allowed alongside --config (the live "
                  "config's `direction` field owns the entry transform); "
                  "edit the config or backtest the strategy by name")
            sys.exit(1)
        close_refs = live_kwargs["close_strategies"]
        # Open strategy name + params come from the live config. Threading
        # params through to run_single_backtest is required — without it,
        # run_single_backtest falls back to the registry default_params and
        # silently ignores per-strategy params from the live config (#643 review #1).
        args.strategy = live_kwargs["open_strategy"]["name"]
        open_params = dict(live_kwargs["open_strategy"]["params"]) or None
        stop_keys = (
            "stop_loss_atr_mult",
            "stop_loss_pct",
            "stop_loss_margin_pct",
            "trailing_stop_atr_mult",
            "trailing_stop_pct",
            "stop_loss_atr_regime",
            "trailing_stop_atr_regime",
            "strategy_type",
            # #942: direction / invert_signal entry transforms, applied to the
            # signal inside Backtester.run (mirrors live invert-then-gate order).
            "direction",
            "invert_signal",
            # #998: regime-profile allocation switch block (None when unused).
            "profile_allocation",
        )
        live_stop_kwargs = {k: live_kwargs[k] for k in stop_keys if k in live_kwargs}

    # CLI ATR-stop flags apply in single mode too; --config refs win on collision.
    if args.stop_loss_atr_mult is not None:
        live_stop_kwargs.setdefault("stop_loss_atr_mult", args.stop_loss_atr_mult)
    if args.trailing_stop_atr_mult is not None:
        live_stop_kwargs.setdefault("trailing_stop_atr_mult", args.trailing_stop_atr_mult)

    # #989 review: --direction was parsed for every mode but forwarded only to
    # optimize — single/compare/multi silently scored the long leg of a
    # requested short run. Forward it to every mode; "both" needs a close
    # evaluator (the plain single-leg path cannot open one side and close the
    # other — Backtester.run rejects it too, as a backstop for API callers).
    if args.direction == "both" and not close_refs \
            and not (args.sweep_close or args.close_stacks_json):
        print("--direction both requires a close evaluator (--close-strategy "
              "or a close-stack sweep); backtest each leg separately with "
              "--direction long / --direction short")
        sys.exit(1)
    # #989 review: optimize mode cannot measure the short leg yet — the
    # walk-forward warmup seeder (optimizer.warmup_exit_long_entry) is
    # long-only, so a carried warmup position would inject a phantom LONG
    # into the short run. walk_forward_optimize rejects it too (backstop for
    # API callers); refuse here before any data fetch and point at the
    # surfaces that do measure the short leg.
    if args.direction == "short" and args.mode == "optimize":
        print("--direction short is not supported in optimize mode (the "
              "walk-forward warmup seeder is long-only and would carry a "
              "phantom long into the short run); use --mode single "
              "--direction short or eval_windows.py --direction short")
        sys.exit(1)
    if args.direction:
        # --config + --direction was rejected above, so the key can't collide.
        live_stop_kwargs["direction"] = args.direction

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
                            allowed_regimes=args.allowed_regimes,
                            **live_stop_kwargs)

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
                           allowed_regimes=args.allowed_regimes,
                           direction=args.direction)

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
                        allowed_regimes=args.allowed_regimes,
                        direction=args.direction)

    elif args.mode == "optimize":
        # #996: close-stack co-optimization. The grid owns the close stack;
        # fixed exit flags alongside it would be silently shadowed — reject.
        close_stack_grid = None
        if args.close_stacks_json or args.sweep_close:
            if close_refs or args.stop_loss_atr_mult is not None \
                    or args.trailing_stop_atr_mult is not None:
                print("--sweep-close/--close-stacks-json is mutually exclusive "
                      "with --close-strategy/--stop-loss-atr-mult/"
                      "--trailing-stop-atr-mult (the grid owns the close stack)")
                sys.exit(1)
            if args.close_stacks_json:
                import json as _json
                with open(args.close_stacks_json) as fh:
                    specs = _json.load(fh)
                if not isinstance(specs, list):
                    print(f"{args.close_stacks_json}: expected a JSON list of "
                          f"close-stack specs")
                    sys.exit(1)
            else:
                specs = DEFAULT_CLOSE_STACK_SPECS
            close_stack_grid = generate_close_stack_grid(specs)
            # #989 review: a no-close (baseline) stack runs the plain
            # single-leg path, which cannot model "both" — the default
            # --sweep-close grid always contains baselines, so reject here
            # before any data fetch instead of tracebacking mid-optimize
            # (walk_forward_optimize raises the same way as a backstop).
            if args.direction == "both" and any(
                    not s.get("close_strategies") for s in close_stack_grid):
                print("--direction both requires a close evaluator on every "
                      "swept close stack, but the grid contains no-close "
                      "baseline stacks (the default --sweep-close grid always "
                      "does); supply --close-stacks-json with close-evaluator "
                      "stacks only, or backtest each leg separately")
                sys.exit(1)

        if args.strategy == "all":
            for strat in reg.list_strategies():
                run_walk_forward(strat, args.symbol, args.timeframe,
                                 args.since, args.splits, args.capital,
                                 registry=args.registry, platform=args.platform,
                                 regime_enabled=args.regime_enabled,
                                 regime_period=args.regime_period,
                                 regime_adx_threshold=args.regime_adx_threshold,
                                 allowed_regimes=args.allowed_regimes,
                                 stop_loss_atr_mult=args.stop_loss_atr_mult,
                                 trailing_stop_atr_mult=args.trailing_stop_atr_mult,
                                 close_strategies=close_refs,
                                 close_stack_grid=close_stack_grid,
                                 optimize_metric=args.optimize_metric,
                                 direction=args.direction)
        else:
            run_walk_forward(args.strategy, args.symbol, args.timeframe,
                             args.since, args.splits, args.capital,
                             registry=args.registry, platform=args.platform,
                             regime_enabled=args.regime_enabled,
                             regime_period=args.regime_period,
                             regime_adx_threshold=args.regime_adx_threshold,
                             allowed_regimes=args.allowed_regimes,
                             stop_loss_atr_mult=args.stop_loss_atr_mult,
                             trailing_stop_atr_mult=args.trailing_stop_atr_mult,
                             close_strategies=close_refs,
                             close_stack_grid=close_stack_grid,
                             optimize_metric=args.optimize_metric,
                             direction=args.direction)


if __name__ == "__main__":
    main()

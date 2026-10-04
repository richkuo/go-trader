#!/usr/bin/env python3

import argparse
import itertools
import json
import math
import os
import statistics
import sys
from typing import List, Optional

sys.path.insert(0, os.path.join(os.path.dirname(__file__), '..', 'shared_tools'))

INCUMBENTS = [
    "momentum_pro",
    "chart_pattern",
    "squeeze_momentum",
    "mean_reversion_pro",
    "donchian_breakout",
    "ichimoku_cloud",
    "range_scalper",
    "sma_crossover",
]

DATASETS = [
    ("BTC/USDT", "1h"),
    ("BTC/USDT", "4h"),
    ("ETH/USDT", "1h"),
    ("ETH/USDT", "4h"),
    ("SOL/USDT", "1h"),
    ("SOL/USDT", "4h"),
]

WINDOWS = {
    "is":     ("2025-06-10", "2026-01-01"),
    "oos":    ("2026-01-01", None),
    "2023":   ("2023-01-01", "2024-01-01"),
    "2024":   ("2024-01-01", "2025-01-01"),
    "2025H1": ("2025-01-01", "2025-07-01"),
}
PROTOCOL_WINDOWS = ("is", "oos")
HELD_OUT_WINDOWS = ("2023", "2024", "2025H1")

DEFAULT_CAPITAL = 1000.0
PLATFORM = "binanceus"

FEE_PLATFORM = "hyperliquid"


def trade_samples_from_results(results: dict) -> List[dict]:
    out = []
    for t in results.get("trades") or []:
        gross = float(t["pnl_pct"])
        notional = float(t.get("shares") or 0.0) * float(t.get("entry_price") or 0.0)
        net = (float(t["pnl"]) / notional * 100.0
               if notional > 0 and t.get("pnl") is not None else gross)
        out.append({"entry_date": str(t["entry_date"]), "pnl_pct": gross,
                    "pnl_pct_net": round(net, 6)})
    return out


def positions_from_results(results: dict) -> List[dict]:
    grouped: dict = {}
    order = []
    for t in results.get("trades") or []:
        key = (str(t["entry_date"]), t.get("side") or "long")
        if key not in grouped:
            grouped[key] = {"entry_date": key[0], "side": key[1],
                            "exit_date": str(t["exit_date"]), "net_pnl": 0.0,
                            "fees": 0.0, "entry_notional": 0.0,
                            "exit_notional": 0.0, "bars_held": 0,
                            "closes": 0, "exit_reasons": []}
            order.append(key)
        g = grouped[key]
        shares = float(t.get("shares") or 0.0)
        g["exit_date"] = str(t["exit_date"])
        g["net_pnl"] += float(t.get("pnl") or 0.0)
        g["fees"] += float(t.get("entry_fee") or 0.0) + float(t.get("exit_fee") or 0.0)
        g["entry_notional"] += shares * float(t.get("entry_price") or 0.0)
        g["exit_notional"] += shares * float(t.get("exit_price") or 0.0)
        g["bars_held"] = max(g["bars_held"], int(t.get("bars_held") or 0))
        g["closes"] += 1
        g["exit_reasons"].append(t.get("exit_reason") or "")
    out = []
    for key in order:
        g = grouped[key]
        g["net_pnl"] = round(g["net_pnl"], 6)
        g["fees"] = round(g["fees"], 6)
        out.append(g)
    return out


def execution_metrics(results: dict, capital: float) -> dict:
    positions = positions_from_results(results)
    fees = sum(p["fees"] for p in positions)
    traded = sum(p["entry_notional"] + p["exit_notional"] for p in positions)
    bars_in_market = sum(p["bars_held"] for p in positions)
    out = {
        "positions": len(positions),
        "long_positions": sum(1 for p in positions if p["side"] == "long"),
        "short_positions": sum(1 for p in positions if p["side"] == "short"),
        "trade_records": len(results.get("trades") or []),
        "fees_usd": round(fees, 6),
        "funding_pnl_usd": float(results.get("total_funding_pnl") or 0.0),
        "traded_notional_usd": round(traded, 6),
        "turnover": round(traded / capital, 6) if capital else None,
        "bars_in_market": bars_in_market,
        "net_pnl_usd": round(float(results.get("final_capital") or 0.0) - capital, 6),
        "position_list": positions,
    }
    execution = results.get("execution")
    if execution:
        out["rejected_entries"] = execution["rejected_entry_count"]
        out["skipped_partial_closes"] = execution["skipped_partial_close_count"]
        out["close_residual_qty"] = execution["close_residual_qty"]
        out["combined_adverse_price_pct"] = execution["combined_adverse_price_pct"]
    return out


def dd_adjusted_return(return_pct: float, max_dd_pct: float) -> float:
    if not max_dd_pct:
        return 0.0
    return return_pct / abs(max_dd_pct)


LIQUIDATED_DDADJ_FLOOR = -100.0


def leg_from_results(results: dict, bh_return_pct: Optional[float] = None) -> dict:
    ret = float(results["total_return_pct"])
    dd = float(results["max_drawdown_pct"])
    liquidated = bool(results.get("liquidated"))
    return {
        "sharpe": float(results["sharpe_ratio"]),
        "return_pct": ret,
        "max_dd_pct": dd,
        "ddadj": (
            LIQUIDATED_DDADJ_FLOOR
            if liquidated
            else round(dd_adjusted_return(ret, dd), 3)
        ),
        "trades": int(results["total_trades"]),
        "bh_return_pct": bh_return_pct,
        "liquidated": liquidated,
    }


def incumbent_bars(incumbent_legs: dict) -> dict:
    bars = {}
    for ds, legs in incumbent_legs.items():
        present = [leg for leg in legs.values() if leg is not None]
        if not present:
            bars[ds] = None
            continue
        bars[ds] = {
            "sharpe": round(statistics.median(l["sharpe"] for l in present), 3),
            "ddadj": round(statistics.median(l["ddadj"] for l in present), 3),
            "n": len(present),
        }
    return bars


def score_candidate(candidate_legs: dict, bars: dict) -> dict:
    rows = []
    for ds in candidate_legs:
        leg = candidate_legs[ds]
        bar = bars.get(ds)
        row = {"dataset": ds, "leg": leg, "bar": bar,
               "beats_sharpe": None, "beats_ddadj": None}
        if leg is not None and bar is not None:
            row["beats_sharpe"] = leg["sharpe"] > bar["sharpe"]
            row["beats_ddadj"] = leg["ddadj"] > bar["ddadj"]
        rows.append(row)

    scored = [r for r in rows if r["leg"] is not None and r["bar"] is not None]
    if not scored:
        return {"rows": rows, "scored_datasets": 0, "verdict": "no data"}

    mean_sharpe = statistics.mean(r["leg"]["sharpe"] for r in scored)
    mean_ddadj = statistics.mean(r["leg"]["ddadj"] for r in scored)
    mean_bar_sharpe = statistics.mean(r["bar"]["sharpe"] for r in scored)
    mean_bar_ddadj = statistics.mean(r["bar"]["ddadj"] for r in scored)
    traded = sum(1 for r in scored if r["leg"]["trades"] > 0)
    liquidated = sum(1 for r in rows
                     if r["leg"] is not None and r["leg"].get("liquidated"))
    degenerate = traded < math.ceil(len(scored) / 2)
    beats_both = (mean_sharpe > mean_bar_sharpe) and (mean_ddadj > mean_bar_ddadj)

    if degenerate:
        verdict = "degenerate"
    elif beats_both:
        verdict = "pass"
    else:
        verdict = "fail"

    return {
        "rows": rows,
        "scored_datasets": len(scored),
        "traded_datasets": traded,
        "mean_sharpe": round(mean_sharpe, 3),
        "mean_ddadj": round(mean_ddadj, 3),
        "mean_bar_sharpe": round(mean_bar_sharpe, 3),
        "mean_bar_ddadj": round(mean_bar_ddadj, 3),
        "beats_sharpe_count": sum(1 for r in scored if r["beats_sharpe"]),
        "beats_ddadj_count": sum(1 for r in scored if r["beats_ddadj"]),
        "liquidated_legs": liquidated,
        "degenerate": degenerate,
        "verdict": verdict,
    }


def parse_sweep_arg(raw: str) -> tuple:
    if "=" not in raw:
        raise ValueError(f"--sweep expects param=v1,v2,...  got: {raw!r}")
    param, _, values = raw.partition("=")
    param = param.strip()
    if not param or not values.strip():
        raise ValueError(f"--sweep expects param=v1,v2,...  got: {raw!r}")
    out = []
    for v in values.split(","):
        v = v.strip()
        try:
            out.append(int(v))
        except ValueError:
            try:
                out.append(float(v))
            except ValueError:
                out.append(v)
    return param, out


def expand_sweep(base_params: dict, sweep_specs: List[tuple]) -> List[tuple]:
    names = [s[0] for s in sweep_specs]
    grids = [s[1] for s in sweep_specs]
    combos = []
    for values in itertools.product(*grids):
        params = dict(base_params)
        params.update(dict(zip(names, values)))
        label = " ".join(f"{n}={v}" for n, v in zip(names, values))
        combos.append((label, params))
    return combos


def dataset_key(symbol: str, timeframe: str) -> str:
    return f"{symbol} {timeframe}"


def parse_dataset_arg(raw: str) -> tuple:
    sym, sep, tf = raw.rpartition(":")
    if not sep or not sym or not tf:
        raise ValueError(f"--datasets expects SYMBOL:TIMEFRAME, got: {raw!r}")
    return sym.strip(), tf.strip()


def run_leg(reg, name: str, params: Optional[dict], symbol: str, timeframe: str,
            window: tuple, capital: float = DEFAULT_CAPITAL,
            close_strategies: Optional[List[dict]] = None,
            direction: Optional[str] = None,
            invert_signal: bool = False,
            stop_loss_atr_mult: Optional[float] = None,
            trailing_stop_atr_mult: Optional[float] = None,
            profile_allocation: Optional[dict] = None,
            allowed_regimes: Optional[list[str]] = None,
            regime_windows_spec: Optional[dict] = None,
            regime_directional_policy: Optional[dict] = None,
            regime_enabled: bool = False,
            regime_period: int = 14,
            regime_adx_threshold: float = 20.0,
            *,
            commission_pct: Optional[float] = None,
            slippage_pct: Optional[float] = None,
            keep_trades: bool = False,
            intrabar_resolution: str = "ohlc_walk",
            exchange_id: Optional[str] = None,
            manifest_ctx: Optional[dict] = None,
            comparison_mode: Optional[str] = None) -> Optional[dict]:
    from atr import ensure_atr_indicator
    import pandas as pd
    from data_fetcher import load_cached_data
    from backtester import Backtester, validate_close_capabilities
    from run_backtest import (FUNDING_COLUMN_STRATEGIES, OBSERVATION_INPUT_STRATEGIES,
                              _attach_funding_if_needed, _build_profile_label_series)

    validate_close_capabilities(close_refs=close_strategies,
                                comparison_mode=comparison_mode,
                                platform=FEE_PLATFORM, phase="preflight")

    manifest_window = None
    manifest_info = None
    execution_spec = None
    observation_params = {}
    if manifest_ctx is None and name in OBSERVATION_INPUT_STRATEGIES:
        raise ValueError(
            f"{name} reads recorded open interest as an entry input; only the --manifest path "
            "(schema offline_candle_manifest/v2 with an open_interest series) can run it")
    if manifest_ctx is not None:
        import offline_manifest as om
        if name in FUNDING_COLUMN_STRATEGIES:
            raise ValueError(
                f"{name} reads funding as an entry input; the frozen manifest path "
                "attaches funding only as a cost")
        if profile_allocation:
            raise ValueError("profile_allocation is not supported on the manifest path")
        if exchange_id is not None:
            raise ValueError("exchange_id and a manifest are mutually exclusive")
        if commission_pct is not None or slippage_pct is not None:
            raise ValueError("the manifest owns fees and slippage; do not also pass "
                             "commission_pct or slippage_pct")
        manifest = manifest_ctx["manifest"]
        dataset = om.dataset_by_key(manifest, dataset_key(symbol, timeframe))
        df, manifest_window, candle_cov = om.window_frame(
            manifest, dataset, manifest_ctx["window"])
        df, funding_cov = om.attach_funding_cost(df, dataset, manifest_window)
        oi_cov = None
        if name in OBSERVATION_INPUT_STRATEGIES:
            if dataset.get("open_interest") is None:
                raise ValueError(
                    f"{name} reads open interest as an entry input; manifest dataset "
                    f"{dataset['key']} attaches no open_interest series")
            oi_obs, oi_cov = om.attach_open_interest(manifest, dataset, manifest_window)
            observation_params = {"open_interest_observations": oi_obs}
        spec = om.execution_spec(manifest, dataset,
                                 manifest_ctx.get("cost_multiplier", 1.0))
        if close_strategies:
            execution_spec = spec
            commission_pct = None
            slippage_pct = None
        else:
            commission_pct = spec["taker_fee_pct"]
            slippage_pct = spec["half_spread_pct"] + spec["slippage_pct"]
        manifest_info = {
            "provenance": dict(manifest["provenance"]),
            "dataset": dataset["key"],
            "candles_sha256": dataset["candles"]["sha256"],
            "funding_sha256": (dataset["funding"] or {}).get("sha256"),
            "candle_coverage": candle_cov,
            "funding_coverage": funding_cov,
            "open_interest_coverage": oi_cov,
            "open_interest_sha256": (dataset.get("open_interest") or {}).get("sha256"),
            "cost_multiplier": manifest_ctx.get("cost_multiplier", 1.0),
            "cost_model": "execution_spec" if execution_spec else "legacy_flat",
            "execution_spec": spec,
        }
    else:
        start, end = window
        load_kwargs = {} if exchange_id is None else {"exchange_id": exchange_id}
        df = load_cached_data(symbol, timeframe, start_date=start, end_date=end,
                              **load_kwargs)
        if df.empty:
            return None
        if end is not None:
            df = df[df.index < pd.Timestamp(end)]
            if df.empty:
                return None
        if name in FUNDING_COLUMN_STRATEGIES:
            df = _attach_funding_if_needed(df, name, symbol, start)

    strat = reg.STRATEGY_REGISTRY.get(name)
    if strat is None:
        raise SystemExit(f"Unknown strategy {name!r}; available: {reg.list_strategies()}")
    strat_params = params if params is not None else strat["default_params"]

    if profile_allocation:
        param_sets = profile_allocation["param_sets"]
        df_signals = None
        for p in sorted(param_sets):
            p_params = {**(strat_params or {}), **(param_sets[p] or {}), **observation_params}
            res = reg.apply_strategy(name, df, p_params)
            if df_signals is None:
                df_signals = res.copy()
                df_signals["signal__" + p] = df_signals.pop("signal")
            else:
                df_signals["signal__" + p] = res["signal"].values
        if close_strategies:
            df_signals = ensure_atr_indicator(df_signals)
        df_signals["_profile_label"] = _build_profile_label_series(
            df_signals, profile_allocation["window_spec"]).values
    else:
        df_signals = reg.apply_strategy(name, df, {**(strat_params or {}), **observation_params}
                                        if observation_params else strat_params)
        if close_strategies:
            df_signals = ensure_atr_indicator(df_signals)

    use_regime = (regime_enabled or bool(allowed_regimes)
                  or bool(regime_windows_spec)
                  or bool(regime_directional_policy))

    indicator_frame = None
    if manifest_window is not None:
        import offline_manifest as om
        if use_regime and "regime" not in df_signals.columns:
            from regime import ensure_regime_columns
            ensure_regime_columns(
                df_signals,
                period=regime_period,
                adx_threshold=regime_adx_threshold,
                windows_spec=regime_windows_spec,
            )
        indicator_frame = df_signals
        df_signals = om.slice_window(df_signals, manifest_window)
        df = om.slice_window(df, manifest_window)
    bt_kwargs = dict(
        initial_capital=capital, platform=FEE_PLATFORM,
        open_strategy={"name": name, "params": dict(strat_params or {})},
        close_strategies=close_strategies,
        direction=direction, invert_signal=invert_signal,
        stop_loss_atr_mult=stop_loss_atr_mult,
        trailing_stop_atr_mult=trailing_stop_atr_mult,
        profile_allocation=profile_allocation,
        regime_enabled=use_regime,
        regime_period=regime_period,
        regime_adx_threshold=regime_adx_threshold,
        allowed_regimes=allowed_regimes,
        regime_windows_spec=regime_windows_spec,
        commission_pct=commission_pct,
        intrabar_resolution=intrabar_resolution,
        comparison_mode=comparison_mode,
    )
    if slippage_pct is not None:
        bt_kwargs["slippage_pct"] = slippage_pct
    if execution_spec is not None:
        bt_kwargs["execution_spec"] = execution_spec
    if regime_directional_policy:
        bt_kwargs["regime_directional_policy"] = regime_directional_policy
        bt_kwargs["regime_directional_certified"] = True
    bt = Backtester(**bt_kwargs)
    results = bt.run(df_signals, strategy_name=name, symbol=symbol,
                     timeframe=timeframe, params=strat_params, save=False,
                     indicator_frame=indicator_frame)
    closes = df["close"].astype(float)
    bh = round((closes.iloc[-1] - closes.iloc[0]) / closes.iloc[0] * 100, 2)
    leg = leg_from_results(results, bh_return_pct=bh)
    try:
        span_days = (df.index[-1] - df.index[0]).total_seconds() / 86400.0
    except (AttributeError, TypeError):
        span_days = None
    leg["span_days"] = round(span_days, 4) if span_days else span_days
    leg["close_validation"] = results.get("close_validation")
    if keep_trades:
        leg["trade_samples"] = trade_samples_from_results(results)
    if manifest_info is not None:
        leg["manifest"] = manifest_info
        leg["execution"] = execution_metrics(results, capital)
    return leg


def compute_incumbent_legs(reg, datasets: List[tuple], window: tuple,
                           capital: float, *,
                           intrabar_resolution: str = "ohlc_walk",
                           manifest_ctx: Optional[dict] = None) -> dict:
    out = {}
    for symbol, timeframe in datasets:
        ds = dataset_key(symbol, timeframe)
        out[ds] = {}
        for name in INCUMBENTS:
            out[ds][name] = run_leg(reg, name, None, symbol, timeframe,
                                    window, capital=capital,
                                    intrabar_resolution=intrabar_resolution,
                                    manifest_ctx=manifest_ctx)
    return out


def manifest_windows(manifest: dict) -> dict:
    return {name: (w["start"], w["end"]) for name, w in manifest["windows"].items()}


def manifest_datasets(manifest: dict) -> List[tuple]:
    return [(d["coin"], manifest["interval"]) for d in manifest["datasets"]]


def validate_candidate(candidate: dict) -> dict:
    if not isinstance(candidate, dict) or not candidate.get("name"):
        raise ValueError("candidate needs a 'name'")
    direction = str(candidate.get("direction") or "long").strip().lower()
    if direction not in ("long", "short", "both"):
        raise ValueError(
            f"candidate direction must be long/short/both, got "
            f"{candidate.get('direction')!r}")
    close_refs = candidate.get("close_strategies")
    if direction == "both" and not close_refs:
        raise ValueError(
            "candidate has direction='both' but no close_strategies. The "
            "plain signal path runs one leg at a time (long/flat, or "
            "short/flat under direction='short'), so the short side of a "
            "'both' candidate would be silently dropped. Add "
            "close_strategies (the open/close engine models both sides) or "
            "evaluate each leg separately.")
    ctype = str(candidate.get("type") or "perps").strip().lower()
    from backtester import validate_close_capabilities
    validate_close_capabilities(close_refs=close_refs,
                                comparison_mode=candidate.get("comparison_mode"),
                                platform=FEE_PLATFORM, strategy_type=ctype,
                                phase="preflight")
    if candidate.get("invert_signal") and ctype not in ("perps", "manual"):
        raise ValueError(
            f"candidate sets invert_signal on type={ctype!r}, but "
            f"invert_signal is HL-perps/manual-only (the live daemon rejects "
            f"this at startup — config.go). Remove invert_signal or declare "
            f"type perps/manual.")
    if candidate.get("stop_loss_atr_mult") and candidate.get("trailing_stop_atr_mult"):
        raise ValueError(
            "candidate sets both stop_loss_atr_mult and "
            "trailing_stop_atr_mult; the stop owners are mutually exclusive "
            "— pick one.")
    pal = candidate.get("profile_allocation")
    if pal:
        from backtester import _parse_profile_allocation
        _parse_profile_allocation(pal)
        if not pal.get("window_spec"):
            raise ValueError(
                "candidate.profile_allocation needs an inline 'window_spec' "
                "({classifier, period[, thresholds|adx_threshold]}) so the "
                "harness can compute the switch label series.")

    ar = candidate.get("allowed_regimes")
    if ar is not None:
        if not isinstance(ar, list):
            raise ValueError(
                "candidate.allowed_regimes must be a list of strings "
                "(or omitted for no gate)")
        if not all(isinstance(x, str) for x in ar):
            raise ValueError(
                "candidate.allowed_regimes entries must all be strings")
        if len(ar) == 0:
            candidate.pop("allowed_regimes", None)

    rws = candidate.get("regime_windows_spec")
    if rws is not None:
        if not isinstance(rws, dict):
            raise ValueError(
                "candidate.regime_windows_spec must be an object "
                "{window_name: {classifier, period, ...}} (or omitted for "
                "the legacy single-lookback ADX gate)")
        if not rws:
            candidate.pop("regime_windows_spec", None)
        else:
            from regime import parse_regime_windows_spec_json
            try:
                normalized = parse_regime_windows_spec_json(json.dumps(rws))
            except (ValueError, TypeError) as exc:
                raise ValueError(f"candidate.regime_windows_spec: {exc}")
            candidate["regime_windows_spec"] = normalized

    rdp = candidate.get("regime_directional_policy")
    if rdp is not None:
        if not isinstance(rdp, dict):
            raise ValueError(
                "candidate.regime_directional_policy must be an object "
                "{trend_regime: {label: {direction, invert_signal?}}} "
                "(or omitted for no directional gate)")
        if not rdp:
            candidate.pop("regime_directional_policy", None)
        else:
            from backtester import _normalize_regime_directional_policy
            try:
                normalized_rdp = _normalize_regime_directional_policy(rdp)
            except (ValueError, TypeError) as exc:
                raise ValueError(
                    f"candidate.regime_directional_policy: {exc}")
            both_labels = sorted(
                label for label, entry in (normalized_rdp or {}).items()
                if entry.get("direction") == "both")
            if both_labels and not close_refs:
                raise ValueError(
                    "candidate.regime_directional_policy resolves "
                    f"direction='both' for {both_labels} but the candidate "
                    "has no close_strategies. The plain signal path runs one "
                    "leg at a time, so a both-sided regime would be rejected "
                    "by the Backtester (or silently mis-scored). Add "
                    "close_strategies (the open/close engine models both "
                    "sides) or drop the both-states.")
            candidate["regime_directional_policy"] = {
                "trend_regime": normalized_rdp}

    rp = candidate.get("regime_period")
    rt = candidate.get("regime_adx_threshold")
    if rp is not None or rt is not None:
        if candidate.get("regime_windows_spec"):
            raise ValueError(
                "candidate sets regime_period/regime_adx_threshold alongside "
                "regime_windows_spec; the windows spec owns the gate's "
                "classifier and lookback — drop the legacy fields")
        if not (candidate.get("allowed_regimes")
                or candidate.get("regime_directional_policy")):
            raise ValueError(
                "candidate sets regime_period/regime_adx_threshold without a "
                "regime gate consumer (allowed_regimes or "
                "regime_directional_policy) — they would be a silent no-op")
        if rp is not None and (isinstance(rp, bool)
                               or not isinstance(rp, int) or rp < 2):
            raise ValueError(
                f"candidate.regime_period must be an int >= 2, got {rp!r}")
        if rt is not None and (isinstance(rt, bool)
                               or not isinstance(rt, (int, float))
                               or not rt > 0):
            raise ValueError(
                "candidate.regime_adx_threshold must be a positive number, "
                f"got {rt!r}")
    return candidate


def run_candidate_leg(reg, candidate: dict, symbol: str, timeframe: str,
                      window: tuple, capital: float = DEFAULT_CAPITAL, *,
                      keep_trades: bool = False,
                      intrabar_resolution: str = "ohlc_walk",
                      manifest_ctx: Optional[dict] = None) -> Optional[dict]:
    return run_leg(
        reg, candidate["name"], candidate.get("params"),
        symbol, timeframe, window, capital=capital,
        close_strategies=candidate.get("close_strategies"),
        direction=candidate.get("direction") or "long",
        invert_signal=bool(candidate.get("invert_signal")),
        stop_loss_atr_mult=candidate.get("stop_loss_atr_mult"),
        trailing_stop_atr_mult=candidate.get("trailing_stop_atr_mult"),
        profile_allocation=candidate.get("profile_allocation"),
        allowed_regimes=candidate.get("allowed_regimes"),
        regime_period=int(candidate.get("regime_period") or 14),
        regime_adx_threshold=float(
            candidate.get("regime_adx_threshold") or 20.0),
        regime_windows_spec=candidate.get("regime_windows_spec"),
        regime_directional_policy=candidate.get("regime_directional_policy"),
        keep_trades=keep_trades,
        intrabar_resolution=intrabar_resolution,
        manifest_ctx=manifest_ctx,
        comparison_mode=candidate.get("comparison_mode"),
    )


def evaluate_window(reg, candidate: dict, datasets: List[tuple],
                    window_name: str, capital: float,
                    bars_memo: dict, *,
                    intrabar_resolution: str = "ohlc_walk",
                    manifest: Optional[dict] = None,
                    cost_multiplier: float = 1.0) -> dict:
    validate_candidate(candidate)
    manifest_ctx = None
    if manifest is not None:
        window = manifest_windows(manifest)[window_name]
        manifest_ctx = {"manifest": manifest, "window": window_name,
                        "cost_multiplier": cost_multiplier}
    else:
        window = WINDOWS[window_name]
    memo_key = (window_name if manifest is None
                else (manifest["path"], window_name, cost_multiplier))
    if memo_key not in bars_memo:
        bars_memo[memo_key] = incumbent_bars(
            compute_incumbent_legs(reg, datasets, window, capital,
                                   intrabar_resolution=intrabar_resolution,
                                   manifest_ctx=manifest_ctx))
    bars = bars_memo[memo_key]

    candidate_legs = {}
    for symbol, timeframe in datasets:
        ds = dataset_key(symbol, timeframe)
        candidate_legs[ds] = run_candidate_leg(
            reg, candidate, symbol, timeframe, window, capital=capital,
            intrabar_resolution=intrabar_resolution,
            manifest_ctx=manifest_ctx)
    score = score_candidate(candidate_legs, bars)
    score["window"] = window_name
    score["window_range"] = list(window)
    score["bars"] = bars
    from backtester import aggregate_close_validations
    score["close_validation"] = aggregate_close_validations(
        (leg or {}).get("close_validation") for leg in candidate_legs.values())
    return score


def _fmt(v, width=8, prec=2):
    if v is None:
        return " " * (width - 1) + "-"
    return f"{v:>{width}.{prec}f}"


def format_window_report(score: dict) -> str:
    start, end = score["window_range"]
    lines = [
        f"\n== window {score['window']} ({start} → {end or 'latest'}) ==",
        f"{'dataset':<14} {'Sharpe':>8} {'bar':>8} {'DDadj':>8} {'bar':>8} "
        f"{'ret%':>8} {'maxDD%':>8} {'B&H%':>8} {'trades':>6}  beats",
    ]
    for row in score["rows"]:
        leg, bar = row["leg"], row["bar"]
        if leg is None:
            lines.append(f"{row['dataset']:<14} {'(no data)'}")
            continue
        beats = ""
        if row["beats_sharpe"] is not None:
            beats = ("S" if row["beats_sharpe"] else "-") + \
                    ("D" if row["beats_ddadj"] else "-")
        if leg.get("liquidated"):
            beats = (beats + " LIQ").strip()
        lines.append(
            f"{row['dataset']:<14} {_fmt(leg['sharpe'])} "
            f"{_fmt(bar['sharpe'] if bar else None)} {_fmt(leg['ddadj'])} "
            f"{_fmt(bar['ddadj'] if bar else None)} {_fmt(leg['return_pct'])} "
            f"{_fmt(leg['max_dd_pct'])} {_fmt(leg['bh_return_pct'])} "
            f"{leg['trades']:>6}  {beats}"
        )
    if score.get("close_validation") is not None:
        from backtester import format_close_validation
        lines.append(format_close_validation(score["close_validation"]))
    if score.get("verdict") == "no data":
        lines.append("verdict: NO DATA")
        return "\n".join(lines)
    lines.append(
        f"{'mean':<14} {_fmt(score['mean_sharpe'])} {_fmt(score['mean_bar_sharpe'])} "
        f"{_fmt(score['mean_ddadj'])} {_fmt(score['mean_bar_ddadj'])}"
    )
    lines.append(
        f"verdict: {score['verdict'].upper()} — beats bar on "
        f"{score['beats_sharpe_count']}/{score['scored_datasets']} (Sharpe), "
        f"{score['beats_ddadj_count']}/{score['scored_datasets']} (DDadj); "
        f"traded {score['traded_datasets']}/{score['scored_datasets']}"
        + (" [degenerate: majority of legs zero-trade]" if score["degenerate"] else "")
        + (f" [{score['liquidated_legs']} liquidated leg(s): equity hit 0, "
           f"metrics floored at the bust bar]"
           if score.get("liquidated_legs") else "")
    )
    return "\n".join(lines)


def format_summary(window_scores: List[dict]) -> str:
    lines = [f"\n== summary ==",
             f"{'window':<10} {'Sharpe':>8} {'bar':>8} {'DDadj':>8} {'bar':>8}  verdict"]
    for s in window_scores:
        if s.get("verdict") == "no data":
            lines.append(f"{s['window']:<10} {'(no data)'}")
            continue
        lines.append(
            f"{s['window']:<10} {_fmt(s['mean_sharpe'])} {_fmt(s['mean_bar_sharpe'])} "
            f"{_fmt(s['mean_ddadj'])} {_fmt(s['mean_bar_ddadj'])}  {s['verdict'].upper()}"
        )
    protocol = [s for s in window_scores if s["window"] in PROTOCOL_WINDOWS]
    held_out = [s for s in window_scores if s["window"] in HELD_OUT_WINDOWS]
    oos = next((s for s in window_scores if s["window"] == "oos"), None)
    if oos is not None and oos.get("verdict") != "no data":
        held_pass = sum(1 for s in held_out if s.get("verdict") == "pass")
        lines.append(
            f"\nprotocol OOS: {oos['verdict'].upper()}"
            + (f"; held-out windows passed: {held_pass}/{len(held_out)}"
               if held_out else "")
        )
    return "\n".join(lines)


def format_sweep_report(sweep_rows: List[dict], window_name: str) -> str:
    lines = [f"\n== plateau sweep (window {window_name}) ==",
             f"{'combo':<32} {'Sharpe':>8} {'bar':>8} {'DDadj':>8} "
             f"{'traded':>7}  verdict"]
    for r in sweep_rows:
        s = r["score"]
        if s.get("verdict") == "no data":
            lines.append(f"{r['label']:<32} {'(no data)'}")
            continue
        lines.append(
            f"{r['label']:<32} {_fmt(s['mean_sharpe'])} {_fmt(s['mean_bar_sharpe'])} "
            f"{_fmt(s['mean_ddadj'])} "
            f"{s['traded_datasets']:>3}/{s['scored_datasets']:<3}  "
            f"{s['verdict'].upper()}"
        )
    lines.append("(M1 step 6: the chosen combo must sit on a broad plateau, "
                 "not a single-param spike.)")
    return "\n".join(lines)


def build_parser() -> argparse.ArgumentParser:
    p = argparse.ArgumentParser(
        description="M1 multi-window incumbent-relative validation (#977)")
    p.add_argument("--strategy", help="Candidate open-strategy name")
    p.add_argument("--params", default=None,
                   help="Candidate params JSON (default: registry default_params)")
    p.add_argument("--candidate-json", default=None,
                   help="Path to a candidate JSON file: {name, params, "
                        "close_strategies?, direction?, invert_signal?, "
                        "stop_loss_atr_mult?, trailing_stop_atr_mult?, "
                        "allowed_regimes?, regime_windows_spec?, "
                        "regime_directional_policy?, profile_allocation?}. "
                        "Overrides --strategy/--params. allowed_regimes enables "
                        "the entry gate on the M1 bar (legacy lookback unless "
                        "regime_windows_spec picks another classifier).")
    p.add_argument("--registry", choices=["spot", "futures"], default="spot")
    p.add_argument("--direction", default=None,
                   choices=["long", "short", "both"],
                   help="Candidate entry side (#989). 'short' runs the "
                        "short/flat mirror path (signal=-1 opens a short, "
                        "+1 closes it) so short-only/bidirectional "
                        "strategies score their short leg instead of zero "
                        "trades. 'both' requires close_strategies (engine "
                        "path). Default: long.")
    p.add_argument("--allowed-regimes", action="append", default=None,
                   metavar="LABEL",
                   help="Regime label to allow entries (repeatable). Enables "
                        "the backtester entry gate for this candidate on the "
                        "M1 incumbent-relative bar (legacy single-lookback "
                        "ADX unless --regime-windows-spec picks another "
                        "classifier).")
    p.add_argument("--regime-windows-spec", default=None, metavar="JSON",
                   help="#985 windows spec JSON selecting the entry gate's "
                        "classifier ({name: {classifier: composite|adx, "
                        "period, ...}}); the Backtester classifies the "
                        "PRIMARY (medium-first) window (#1058) so composite "
                        "labels work in --allowed-regimes. Omit for the "
                        "legacy ADX gate.")
    p.add_argument("--regime-directional-policy", default=None, metavar="JSON",
                   help="#1166 directional-policy JSON (#1025 shape: "
                        "{trend_regime: {label: {direction: long|short|both, "
                        "invert_signal?}}}) resolving the entry direction per "
                        "bar regime. RESEARCH MODE: the leg passes the #1085 "
                        "certified input explicitly, so the default-off live "
                        "gate is bypassed deliberately for measurement. "
                        "both-states require close_strategies.")
    p.add_argument("--windows", default=None,
                   help=f"Comma list of windows (default: all). "
                        f"Known: {', '.join(WINDOWS)}")
    p.add_argument("--datasets", default=None,
                   help="Comma list of SYMBOL:TIMEFRAME (default: the six "
                        "audit datasets)")
    p.add_argument("--capital", type=float, default=DEFAULT_CAPITAL)
    p.add_argument("--sweep", action="append", default=None, metavar="P=V1,V2",
                   help="Plateau sweep over a param (repeatable; cartesian)")
    p.add_argument("--sweep-window", default=None,
                   help="Window the sweep is scored on (default: oos; with "
                        "--manifest it must be named explicitly, e.g. the "
                        "declared training window)")
    p.add_argument("--manifest", default=None, metavar="PATH",
                   help="#1649 frozen offline manifest (offline_manifest.py): "
                        "datasets, finite windows, warm-up, venue candles, "
                        "funding cost, fees, spread/slippage and lot/minimum "
                        "rules come from the manifest; hashes are verified and "
                        "no network fetch happens. Default: off (legacy loader).")
    p.add_argument("--cost-multiplier", type=float, default=1.0,
                   help="With --manifest: scale spread and slippage (cost "
                        "sensitivity). Default 1.0.")
    p.add_argument("--profile-allocation", default=None,
                   help="#998 regime-profile allocation JSON: {window_spec:"
                        "{classifier,period,...}, profiles:{label:profile}, "
                        "param_sets:{profile:{...}}, confirm_bars, initial_profile}. "
                        "Scores the switched composite on the same harness.")
    p.add_argument("--json", default=None, dest="json_out",
                   help="Write the full structured result to this path")
    p.add_argument("--intrabar-resolution", dest="intrabar_resolution",
                   choices=["ohlc_walk", "bar_close"], default="ohlc_walk",
                   help="SL race resolution (#1271): ohlc_walk (default) or "
                        "bar_close (reproduce pre-#1271 documented baselines).")
    p.add_argument("--comparison-mode", dest="comparison_mode", default=None,
                   metavar="MODE",
                   help="#1683 close comparison mode for the candidate. Omitted = "
                        "strict (refuses time_stop/zscore_target, whose live "
                        "inputs no platform supplies, and HL-live-only closes). "
                        "'approximate' opts research into those exits; every leg, "
                        "window and the JSON payload carry close_validation with "
                        "incomplete parity. Overrides nothing: a candidate JSON "
                        "comparison_mode that differs is an error.")
    return p


def main(argv: Optional[List[str]] = None) -> int:
    args = build_parser().parse_args(argv)

    if args.candidate_json:
        with open(args.candidate_json) as fh:
            candidate = json.load(fh)
        if not isinstance(candidate, dict) or not candidate.get("name"):
            raise SystemExit(f"{args.candidate_json}: candidate JSON needs a 'name'")
    elif args.strategy:
        candidate = {"name": args.strategy}
        if args.params:
            candidate["params"] = json.loads(args.params)
    else:
        raise SystemExit("supply --strategy or --candidate-json")

    if args.profile_allocation:
        candidate["profile_allocation"] = json.loads(args.profile_allocation)

    if args.direction:
        candidate["direction"] = args.direction

    if args.allowed_regimes:
        candidate["allowed_regimes"] = list(args.allowed_regimes)

    if args.regime_windows_spec:
        candidate["regime_windows_spec"] = json.loads(args.regime_windows_spec)

    if args.regime_directional_policy:
        candidate["regime_directional_policy"] = json.loads(
            args.regime_directional_policy)

    if args.comparison_mode is not None:
        if "comparison_mode" in candidate and candidate["comparison_mode"] != args.comparison_mode:
            raise SystemExit(
                f"--comparison-mode {args.comparison_mode!r} conflicts with the candidate "
                f"JSON comparison_mode {candidate['comparison_mode']!r}; pick one")
        candidate["comparison_mode"] = args.comparison_mode

    try:
        validate_candidate(candidate)
    except ValueError as exc:
        raise SystemExit(str(exc))

    manifest = None
    known_windows = WINDOWS
    if args.manifest:
        import offline_manifest as om
        try:
            manifest = om.load_manifest(args.manifest)
        except om.ManifestError as exc:
            raise SystemExit(f"manifest error: {exc}")
        known_windows = manifest_windows(manifest)
        if args.datasets:
            raise SystemExit("--datasets and --manifest are mutually exclusive; "
                             "the manifest owns the datasets")
        if args.sweep and not args.sweep_window:
            raise SystemExit("--manifest sweeps need an explicit --sweep-window "
                             f"(known: {list(known_windows)})")
    elif args.cost_multiplier != 1.0:
        raise SystemExit("--cost-multiplier needs --manifest")
    sweep_window = args.sweep_window or "oos"
    if args.sweep and sweep_window not in known_windows:
        raise SystemExit(f"unknown --sweep-window {sweep_window!r}; known: {list(known_windows)}")

    if args.windows:
        window_names = [w.strip() for w in args.windows.split(",") if w.strip()]
        unknown = [w for w in window_names if w not in known_windows]
        if unknown:
            raise SystemExit(f"unknown windows {unknown}; known: {list(known_windows)}")
    else:
        window_names = list(known_windows)

    if manifest is not None:
        datasets = manifest_datasets(manifest)
    elif args.datasets:
        datasets = [parse_dataset_arg(d) for d in args.datasets.split(",") if d.strip()]
    else:
        datasets = list(DATASETS)

    from registry_loader import load_registry
    reg = load_registry(args.registry)

    print(f"candidate: {candidate['name']} "
          f"(params: {candidate.get('params') or 'registry defaults'}, "
          f"registry: {args.registry})")
    print(f"incumbent bar: median of {len(INCUMBENTS)} incumbents, "
          f"recomputed per (window, dataset)")

    bars_memo: dict = {}
    window_scores = []
    for wname in window_names:
        score = evaluate_window(reg, candidate, datasets, wname,
                                args.capital, bars_memo,
                                intrabar_resolution=args.intrabar_resolution,
                                manifest=manifest,
                                cost_multiplier=args.cost_multiplier)
        window_scores.append(score)
        print(format_window_report(score))

    print(format_summary(window_scores))
    from backtester import aggregate_close_validations, format_close_validation
    print(format_close_validation(aggregate_close_validations(
        s.get("close_validation") for s in window_scores)))

    sweep_rows = []
    if args.sweep:
        specs = [parse_sweep_arg(s) for s in args.sweep]
        base = dict(candidate.get("params") or {})
        for label, params in expand_sweep(base, specs):
            combo = dict(candidate)
            combo["params"] = params
            score = evaluate_window(reg, combo, datasets, sweep_window,
                                    args.capital, bars_memo,
                                    intrabar_resolution=args.intrabar_resolution,
                                    manifest=manifest,
                                    cost_multiplier=args.cost_multiplier)
            sweep_rows.append({"label": label, "params": params, "score": score})
        print(format_sweep_report(sweep_rows, sweep_window))

    if args.json_out:
        manifest_meta = None
        if manifest is not None:
            import offline_manifest as om
            manifest_meta = {
                "path": os.path.relpath(manifest["path"]),
                "sha256": om.sha256_file(manifest["path"]),
                "provenance": manifest["provenance"],
                "cost_multiplier": args.cost_multiplier,
            }
        payload = {
            "candidate": candidate,
            "registry": args.registry,
            "incumbents": INCUMBENTS,
            "datasets": [dataset_key(s, t) for s, t in datasets],
            "windows": {w: list(known_windows[w]) for w in window_names},
            "manifest": manifest_meta,
            "window_scores": window_scores,
            "sweep": sweep_rows,
            "close_validation": aggregate_close_validations(
                [s.get("close_validation") for s in window_scores]
                + [r["score"].get("close_validation") for r in sweep_rows]),
        }
        with open(args.json_out, "w") as fh:
            json.dump(payload, fh, indent=2, default=str)
        print(f"\nwrote {args.json_out}")
    return 0


if __name__ == "__main__":
    sys.exit(main())

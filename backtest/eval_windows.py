#!/usr/bin/env python3
"""
eval_windows.py — M1 multi-window incumbent-relative validation harness (#977).

One command per application issue (#979-#993): runs a candidate strategy config
across the audit datasets and protocol/held-out windows, recomputes the
incumbent-median bar per (window, dataset) on the identical harness, and emits
per-dataset Sharpe / return / max-DD / DD-adjusted-return / trades, pass/fail
verdicts, and optional plateau sweeps.

Harness is audit-identical (#956/#963/#976): registry default or supplied
params, single mode, binanceus fee model, long-leg signal path unless the
candidate config supplies close refs / direction. Sharpe uses the Backtester's
timeframe-annualized scale, the same scale on both sides of the bar.

The incumbent set and window definitions are versioned HERE so bars stay
reproducible as data accrues — change them only with a corresponding note on
#977.

Examples:
  # Full protocol + held-out verdict table for a candidate with default params
  uv run --no-sync python backtest/eval_windows.py --strategy regime_adaptive_htf

  # Explicit params, futures registry, OOS window only
  uv run --no-sync python backtest/eval_windows.py --strategy breakout \\
      --registry futures --params '{"period": 20}' --windows oos

  # Plateau sweep (M1 step 6) over htf_factor on the protocol OOS window
  uv run --no-sync python backtest/eval_windows.py --strategy regime_adaptive_htf \\
      --sweep htf_factor=4,5,6,8,10,12
"""

import argparse
import itertools
import json
import math
import os
import statistics
import sys
from typing import List, Optional

sys.path.insert(0, os.path.join(os.path.dirname(__file__), '..', 'shared_tools'))

# ---------------------------------------------------------------------------
# Versioned harness definitions (#977). The incumbent set is the #963/#976
# eight: the #956 audit's OOS top-8 with futures-only `breakout` excluded from
# the long-leg harness and `sma_crossover` (next-ranked "both" strategy) in its
# slot. All eight exist byte-identical in both registries, so the bar is valid
# for spot and futures candidates alike.
# ---------------------------------------------------------------------------
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

# The six audit datasets (#956): BTC/ETH/SOL x 1h/4h.
DATASETS = [
    ("BTC/USDT", "1h"),
    ("BTC/USDT", "4h"),
    ("ETH/USDT", "1h"),
    ("ETH/USDT", "4h"),
    ("SOL/USDT", "1h"),
    ("SOL/USDT", "4h"),
]

# (start, end) date strings; end=None means "latest cached bar". The protocol
# split mirrors #963/#976 (IS since 2025-06-10, OOS since 2026-01-01); the
# held-out windows (M1 step 7) were never used during any incumbent's design.
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
PLATFORM = "binanceus"  # audit fee model; fixed, not a knob


# ---------------------------------------------------------------------------
# Pure scoring helpers (unit-tested without data access).
# ---------------------------------------------------------------------------

def dd_adjusted_return(return_pct: float, max_dd_pct: float) -> float:
    """DDadj = total return / |max drawdown| (#963 definition).

    A leg with zero drawdown carries no risk denominator; return 0.0 so an
    untraded leg never inflates the mean (zero-trade legs are additionally
    flagged degenerate in score_candidate).
    """
    if not max_dd_pct:
        return 0.0
    return return_pct / abs(max_dd_pct)


def leg_from_results(results: dict, bh_return_pct: Optional[float] = None) -> dict:
    """Collapse a Backtester result dict to the per-leg metrics M1 reports."""
    ret = float(results["total_return_pct"])
    dd = float(results["max_drawdown_pct"])
    return {
        "sharpe": float(results["sharpe_ratio"]),
        "return_pct": ret,
        "max_dd_pct": dd,
        "ddadj": round(dd_adjusted_return(ret, dd), 3),
        "trades": int(results["total_trades"]),
        "bh_return_pct": bh_return_pct,
    }


def incumbent_bars(incumbent_legs: dict) -> dict:
    """Per-dataset incumbent-median bars (M1 step 2).

    ``incumbent_legs``: {dataset_key: {incumbent_name: leg | None}}.
    Returns {dataset_key: {"sharpe": median, "ddadj": median, "n": count}}
    over incumbents that produced a leg; a dataset where no incumbent ran
    yields None (no bar — the candidate leg is reported but unscored).
    """
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
    """Verdict for one window (M1 steps 2 + 5).

    Pass = candidate mean beats the mean per-dataset bar on BOTH Sharpe and
    DDadj, across datasets where both sides ran, AND the result is not
    degenerate (must trade on a majority of scored datasets — #976 rejected
    zero-trade-leg passes as meaningless).
    """
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
        "degenerate": degenerate,
        "verdict": verdict,
    }


def parse_sweep_arg(raw: str) -> tuple:
    """Parse 'param=v1,v2,v3' into (param, [values]) with numeric coercion."""
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
    """Cartesian product of sweep values over base params.

    Returns [(label, params), ...]; label names only the swept params so the
    plateau table stays readable.
    """
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
    """Parse 'BTC/USDT:1h' into (symbol, timeframe)."""
    sym, sep, tf = raw.rpartition(":")
    if not sep or not sym or not tf:
        raise ValueError(f"--datasets expects SYMBOL:TIMEFRAME, got: {raw!r}")
    return sym.strip(), tf.strip()


# ---------------------------------------------------------------------------
# Leg execution (I/O; everything above stays pure for tests).
# ---------------------------------------------------------------------------

def run_leg(reg, name: str, params: Optional[dict], symbol: str, timeframe: str,
            window: tuple, capital: float = DEFAULT_CAPITAL,
            close_strategies: Optional[List[dict]] = None,
            direction: Optional[str] = None,
            invert_signal: bool = False,
            stop_loss_atr_mult: Optional[float] = None,
            trailing_stop_atr_mult: Optional[float] = None,
            profile_allocation: Optional[dict] = None,
            *,
            commission_pct: Optional[float] = None,
            slippage_pct: Optional[float] = None) -> Optional[dict]:
    """Run one (strategy, dataset, window) leg on the audit-identical harness.

    ``commission_pct`` / ``slippage_pct`` are keyword-only friction overrides
    (default ``None``): with both ``None`` the harness is byte-identical to the
    M1 scorer (platform fee + the Backtester's 5 bps slippage default). The fee
    audit (#999) passes ``commission_pct=0.0, slippage_pct=0.0`` for the gross
    (zero-friction) re-run. The returned leg dict carries an additive
    ``span_days`` key (calendar span of the data slice) so callers can
    annualize trade counts.
    """
    from atr import ensure_atr_indicator
    from data_fetcher import load_cached_data
    from backtester import Backtester
    from run_backtest import (FUNDING_COLUMN_STRATEGIES, _attach_funding_if_needed,
                              _build_profile_label_series)

    start, end = window
    df = load_cached_data(symbol, timeframe, start_date=start, end_date=end)
    if df.empty:
        return None
    if name in FUNDING_COLUMN_STRATEGIES:
        df = _attach_funding_if_needed(df, name, symbol, start)

    strat = reg.STRATEGY_REGISTRY.get(name)
    if strat is None:
        raise SystemExit(f"Unknown strategy {name!r}; available: {reg.list_strategies()}")
    strat_params = params if params is not None else strat["default_params"]

    if profile_allocation:
        # #998: per-profile signals + long-window label, then engine replays the switch.
        param_sets = profile_allocation["param_sets"]
        df_signals = None
        for p in sorted(param_sets):
            p_params = {**(strat_params or {}), **(param_sets[p] or {})}
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
        df_signals = reg.apply_strategy(name, df, strat_params)
        if close_strategies:
            df_signals = ensure_atr_indicator(df_signals)

    bt_kwargs = dict(
        initial_capital=capital, platform=PLATFORM,
        open_strategy={"name": name, "params": dict(strat_params or {})},
        close_strategies=close_strategies,
        direction=direction, invert_signal=invert_signal,
        stop_loss_atr_mult=stop_loss_atr_mult,
        trailing_stop_atr_mult=trailing_stop_atr_mult,
        profile_allocation=profile_allocation,
        # commission_pct=None keeps the Backtester's platform-derived fee — the
        # M1 default; an explicit 0.0 (fee audit gross run) overrides it.
        commission_pct=commission_pct,
    )
    # Only override slippage when asked; otherwise the Backtester's 5 bps
    # default stands (passing None would zero it out via the constructor).
    if slippage_pct is not None:
        bt_kwargs["slippage_pct"] = slippage_pct
    bt = Backtester(**bt_kwargs)
    results = bt.run(df_signals, strategy_name=name, symbol=symbol,
                     timeframe=timeframe, params=strat_params, save=False)
    closes = df["close"].astype(float)
    bh = round((closes.iloc[-1] - closes.iloc[0]) / closes.iloc[0] * 100, 2)
    leg = leg_from_results(results, bh_return_pct=bh)
    try:
        span_days = (df.index[-1] - df.index[0]).total_seconds() / 86400.0
    except (AttributeError, TypeError):
        span_days = None
    leg["span_days"] = round(span_days, 4) if span_days else span_days
    return leg


def compute_incumbent_legs(reg, datasets: List[tuple], window: tuple,
                           capital: float) -> dict:
    """All incumbent legs for one window: {dataset_key: {name: leg|None}}."""
    out = {}
    for symbol, timeframe in datasets:
        ds = dataset_key(symbol, timeframe)
        out[ds] = {}
        for name in INCUMBENTS:
            out[ds][name] = run_leg(reg, name, None, symbol, timeframe,
                                    window, capital=capital)
    return out


def validate_candidate(candidate: dict) -> dict:
    """Reject candidate configs the harness cannot faithfully model.

    Mirrors the run_backtest.py --config guards: the plain long/flat signal
    path (no close evaluator) cannot open shorts (signal=-1 only closes a
    long), so a short/both direction there would silently score as long/flat
    — a misleading verdict, not an error. Likewise invert_signal is
    HL-perps/manual-only live (config.go rejects it elsewhere at startup), so
    a candidate declaring another type must not have signals flipped on a
    path the live daemon would refuse to load.
    """
    if not isinstance(candidate, dict) or not candidate.get("name"):
        raise ValueError("candidate needs a 'name'")
    direction = str(candidate.get("direction") or "long").strip().lower()
    if direction not in ("long", "short", "both"):
        raise ValueError(
            f"candidate direction must be long/short/both, got "
            f"{candidate.get('direction')!r}")
    close_refs = candidate.get("close_strategies")
    if direction in ("short", "both") and not close_refs:
        raise ValueError(
            f"candidate has direction={direction!r} but no close_strategies. "
            f"The plain long/flat signal path cannot open shorts (signal=-1 "
            f"only closes a long), so the short side would be silently "
            f"dropped. Add close_strategies (the open/close engine models "
            f"both sides) or evaluate a long-only variant.")
    ctype = str(candidate.get("type") or "perps").strip().lower()
    if candidate.get("invert_signal") and ctype not in ("perps", "manual"):
        raise ValueError(
            f"candidate sets invert_signal on type={ctype!r}, but "
            f"invert_signal is HL-perps/manual-only (the live daemon rejects "
            f"this at startup — config.go). Remove invert_signal or declare "
            f"type perps/manual.")
    # #996: backtester-level ATR stop owners are mutually exclusive (mirrors
    # the live config's exclusive stop fields).
    if candidate.get("stop_loss_atr_mult") and candidate.get("trailing_stop_atr_mult"):
        raise ValueError(
            "candidate sets both stop_loss_atr_mult and "
            "trailing_stop_atr_mult; the stop owners are mutually exclusive "
            "— pick one.")
    # #998: regime-profile allocation is backtestable; validate the block shape
    # and require an inline window_spec so the harness can compute the label
    # series (eval_windows has no live regime store / config).
    pal = candidate.get("profile_allocation")
    if pal:
        from backtester import _parse_profile_allocation
        _parse_profile_allocation(pal)  # raises on bad param_sets/confirm/initial
        if not pal.get("window_spec"):
            raise ValueError(
                "candidate.profile_allocation needs an inline 'window_spec' "
                "({classifier, period[, thresholds|adx_threshold]}) so the "
                "harness can compute the switch label series.")
    return candidate


def evaluate_window(reg, candidate: dict, datasets: List[tuple],
                    window_name: str, capital: float,
                    bars_memo: dict) -> dict:
    """Candidate legs + incumbent bars + verdict for one window."""
    validate_candidate(candidate)
    window = WINDOWS[window_name]
    if window_name not in bars_memo:
        bars_memo[window_name] = incumbent_bars(
            compute_incumbent_legs(reg, datasets, window, capital))
    bars = bars_memo[window_name]

    candidate_legs = {}
    for symbol, timeframe in datasets:
        ds = dataset_key(symbol, timeframe)
        candidate_legs[ds] = run_leg(
            reg, candidate["name"], candidate.get("params"),
            symbol, timeframe, window, capital=capital,
            close_strategies=candidate.get("close_strategies"),
            # The validated default ("long") must also be the EXECUTED
            # default: with close refs and direction=None the engine path
            # would open shorts on raw signal=-1, silently scoring a
            # different entry universe than the long-leg harness (#996).
            direction=candidate.get("direction") or "long",
            invert_signal=bool(candidate.get("invert_signal")),
            stop_loss_atr_mult=candidate.get("stop_loss_atr_mult"),
            trailing_stop_atr_mult=candidate.get("trailing_stop_atr_mult"),
            profile_allocation=candidate.get("profile_allocation"),
        )
    score = score_candidate(candidate_legs, bars)
    score["window"] = window_name
    score["window_range"] = list(window)
    score["bars"] = bars
    return score


# ---------------------------------------------------------------------------
# Reporting.
# ---------------------------------------------------------------------------

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
        lines.append(
            f"{row['dataset']:<14} {_fmt(leg['sharpe'])} "
            f"{_fmt(bar['sharpe'] if bar else None)} {_fmt(leg['ddadj'])} "
            f"{_fmt(bar['ddadj'] if bar else None)} {_fmt(leg['return_pct'])} "
            f"{_fmt(leg['max_dd_pct'])} {_fmt(leg['bh_return_pct'])} "
            f"{leg['trades']:>6}  {beats}"
        )
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


# ---------------------------------------------------------------------------
# CLI.
# ---------------------------------------------------------------------------

def build_parser() -> argparse.ArgumentParser:
    p = argparse.ArgumentParser(
        description="M1 multi-window incumbent-relative validation (#977)")
    p.add_argument("--strategy", help="Candidate open-strategy name")
    p.add_argument("--params", default=None,
                   help="Candidate params JSON (default: registry default_params)")
    p.add_argument("--candidate-json", default=None,
                   help="Path to a candidate JSON file: {name, params, "
                        "close_strategies?, direction?, invert_signal?, "
                        "stop_loss_atr_mult?, trailing_stop_atr_mult?}. "
                        "Overrides --strategy/--params.")
    p.add_argument("--registry", choices=["spot", "futures"], default="spot")
    p.add_argument("--windows", default=None,
                   help=f"Comma list of windows (default: all). "
                        f"Known: {', '.join(WINDOWS)}")
    p.add_argument("--datasets", default=None,
                   help="Comma list of SYMBOL:TIMEFRAME (default: the six "
                        "audit datasets)")
    p.add_argument("--capital", type=float, default=DEFAULT_CAPITAL)
    p.add_argument("--sweep", action="append", default=None, metavar="P=V1,V2",
                   help="Plateau sweep over a param (repeatable; cartesian)")
    p.add_argument("--sweep-window", default="oos", choices=list(WINDOWS),
                   help="Window the sweep is scored on (default: oos)")
    p.add_argument("--profile-allocation", default=None,
                   help="#998 regime-profile allocation JSON: {window_spec:"
                        "{classifier,period,...}, profiles:{label:profile}, "
                        "param_sets:{profile:{...}}, confirm_bars, initial_profile}. "
                        "Scores the switched composite on the same harness.")
    p.add_argument("--json", default=None, dest="json_out",
                   help="Write the full structured result to this path")
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

    try:
        validate_candidate(candidate)
    except ValueError as exc:
        raise SystemExit(str(exc))

    if args.windows:
        window_names = [w.strip() for w in args.windows.split(",") if w.strip()]
        unknown = [w for w in window_names if w not in WINDOWS]
        if unknown:
            raise SystemExit(f"unknown windows {unknown}; known: {list(WINDOWS)}")
    else:
        window_names = list(WINDOWS)

    if args.datasets:
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
                                args.capital, bars_memo)
        window_scores.append(score)
        print(format_window_report(score))

    print(format_summary(window_scores))

    sweep_rows = []
    if args.sweep:
        specs = [parse_sweep_arg(s) for s in args.sweep]
        base = dict(candidate.get("params") or {})
        for label, params in expand_sweep(base, specs):
            combo = dict(candidate)
            combo["params"] = params
            score = evaluate_window(reg, combo, datasets, args.sweep_window,
                                    args.capital, bars_memo)
            sweep_rows.append({"label": label, "params": params, "score": score})
        print(format_sweep_report(sweep_rows, args.sweep_window))

    if args.json_out:
        payload = {
            "candidate": candidate,
            "registry": args.registry,
            "incumbents": INCUMBENTS,
            "datasets": [dataset_key(s, t) for s, t in datasets],
            "windows": {w: list(WINDOWS[w]) for w in window_names},
            "window_scores": window_scores,
            "sweep": sweep_rows,
        }
        with open(args.json_out, "w") as fh:
            json.dump(payload, fh, indent=2, default=str)
        print(f"\nwrote {args.json_out}")
    return 0


if __name__ == "__main__":
    sys.exit(main())

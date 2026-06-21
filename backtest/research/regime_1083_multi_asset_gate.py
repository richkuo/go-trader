"""Multi-asset breadth gate for regime-volatility promotion (#1083).

This is a thin research-layer orchestrator over the two single-cell gates:

  1. #1080 ``run_bakeoff`` selects a gate-passing volatility-state model for one
     (symbol, timeframe) cell.
  2. The selected family/K is refit on that same cell's in-sample window and
     passed to #1081 ``run_gate`` for regime-conditioned ATR economics.

No gate logic lives here. This wrapper owns only the audit-universe loop, the
per-cell model handoff, failure reporting, JSON output, and the promotion
aggregation rule.
"""
from __future__ import annotations

import argparse
import json
import os
import sys
from typing import Callable, Iterable, Optional

_THIS_DIR = os.path.dirname(os.path.abspath(__file__))
_BACKTEST = os.path.abspath(os.path.join(_THIS_DIR, ".."))
_ROOT = os.path.abspath(os.path.join(_BACKTEST, ".."))
for _p in (_THIS_DIR, _BACKTEST, _ROOT, os.path.join(_ROOT, "shared_tools")):
    if _p not in sys.path:
        sys.path.insert(0, _p)

from data_fetcher import load_cached_data  # noqa: E402
from eval_windows import DATASETS, DEFAULT_CAPITAL, PLATFORM, WINDOWS  # noqa: E402
from regime import _DEFAULT_COMPOSITE_THRESHOLDS, composite_feature_matrix  # noqa: E402
import regime_vol_model as rvm  # noqa: E402
from regime_1080_unsupervised_vol_model import (  # noqa: E402
    DEFAULT_WINDOWS as BAKEOFF_DEFAULT_WINDOWS,
    run_bakeoff,
)
from regime_1081_economic_gate import (  # noqa: E402
    DEFAULT_DIRECTION,
    DEFAULT_GATE_WINDOWS,
    DEFAULT_REGISTRY,
    DEFAULT_STRATEGY,
    DEFAULT_WINDOWS as ECONOMIC_DEFAULT_WINDOWS,
    SURFACES,
    format_report as format_economic_report,
    run_gate,
)


Dataset = tuple[str, str]


def dataset_label(symbol: str, timeframe: str) -> str:
    return f"{symbol} {timeframe}"


def parse_datasets(raw: str | Iterable[str] | None) -> list[Dataset]:
    """Parse ``BTC/USDT:1h,ETH/USDT:4h``; empty means eval_windows.DATASETS."""
    if raw is None:
        return list(DATASETS)
    if isinstance(raw, str):
        if not raw.strip():
            return list(DATASETS)
        parts = [p.strip() for p in raw.split(",")]
    else:
        parts = [str(p).strip() for p in raw]
    out: list[Dataset] = []
    for part in parts:
        if not part:
            continue
        if ":" not in part:
            raise ValueError(f"dataset {part!r} must be SYMBOL:TIMEFRAME")
        symbol, timeframe = part.rsplit(":", 1)
        symbol, timeframe = symbol.strip(), timeframe.strip()
        if not symbol or not timeframe:
            raise ValueError(f"dataset {part!r} must include both symbol and timeframe")
        out.append((symbol, timeframe))
    if not out:
        raise ValueError("at least one dataset is required")
    return out


def parse_csv(raw: str | Iterable[str] | None) -> list[str]:
    if raw is None:
        return []
    if isinstance(raw, str):
        parts = raw.split(",")
    else:
        parts = [str(x) for x in raw]
    return [p.strip() for p in parts if p.strip()]


def parse_ints(raw: str | Iterable[int] | None) -> list[int]:
    if raw is None:
        return []
    if isinstance(raw, str):
        parts = raw.split(",")
    else:
        parts = [str(x) for x in raw]
    vals = [int(p.strip()) for p in parts if p.strip()]
    if any(v < 2 for v in vals):
        raise ValueError("latent state counts must be >= 2")
    return vals


def _summarize_bakeoff(report: dict) -> dict:
    return {
        "winner": report.get("winner"),
        "candidate_count": report.get("candidate_count"),
        "target": report.get("target"),
        "handrule_held_out": report.get("handrule_held_out"),
        "significance_alpha": report.get("significance_alpha"),
        "bonferroni_alpha": report.get("bonferroni_alpha"),
    }


def fit_selected_model(
    symbol: str,
    timeframe: str,
    winner: dict,
    *,
    in_sample: str,
    period: int,
    filter_window: int,
    seed: int,
) -> dict:
    """Refit the #1080-selected model so #1081 can consume the actual artifact."""
    family = str(winner.get("family") or "").strip()
    k = int(winner.get("k") or 0)
    if not family or k < 2:
        raise ValueError(f"invalid #1080 winner payload: {winner!r}")
    start, end = WINDOWS[in_sample]
    df = load_cached_data(symbol, timeframe, exchange_id=PLATFORM,
                          start_date=start, end_date=end)
    if df.empty:
        raise ValueError(f"{dataset_label(symbol, timeframe)} {in_sample}: no cached data")
    th = dict(_DEFAULT_COMPOSITE_THRESHOLDS)
    feats = composite_feature_matrix(df, period, th).to_numpy()
    return rvm.fit_unsupervised(
        feats,
        family=family,
        k=k,
        filter_window=filter_window,
        period=period,
        thresholds=th,
        seed=seed,
        fitted_on={"symbol": symbol, "timeframe": timeframe, "window": in_sample},
    )


def _cell_blocking_reasons(row: dict) -> list[str]:
    reasons = list(row.get("blocking_reasons") or [])
    econ = row.get("economic_report") or {}
    reasons.extend(econ.get("summary", {}).get("blocking_reasons", []) or [])
    if not reasons and row.get("error"):
        reasons.append(str(row["error"]))
    if not reasons and not row.get("pass"):
        reasons.append("cell did not pass")
    return reasons


def run_cell(
    symbol: str,
    timeframe: str,
    *,
    in_sample: str,
    held_out: str,
    bakeoff_windows: Iterable[str],
    economic_windows: Iterable[str],
    gate_windows: Iterable[str],
    surfaces: Iterable[str],
    families: Iterable[str],
    k_range: Iterable[int],
    period: int,
    filter_window: int,
    seed: int,
    strategy: str,
    registry: str,
    direction: str,
    params: Optional[dict],
    capital: float,
    thresholds: Optional[dict] = None,
    bakeoff_fn: Callable[..., dict] = run_bakeoff,
    fit_model_fn: Callable[..., dict] = fit_selected_model,
    economic_gate_fn: Callable[..., dict] = run_gate,
) -> dict:
    row = {
        "symbol": symbol,
        "timeframe": timeframe,
        "dataset": dataset_label(symbol, timeframe),
        "pass": False,
        "blocking_reasons": [],
    }
    try:
        bakeoff = bakeoff_fn(
            symbol,
            timeframe,
            in_sample=in_sample,
            held_out=held_out,
            eval_windows=tuple(bakeoff_windows),
            families=tuple(families),
            k_range=tuple(k_range),
            period=period,
            filter_window=filter_window,
            seed=seed,
        )
        row["bakeoff"] = _summarize_bakeoff(bakeoff)
        winner = bakeoff.get("winner")
        if not winner:
            row["blocking_reasons"].append("no #1080 gate-passing model")
            return row
        model = fit_model_fn(
            symbol,
            timeframe,
            winner,
            in_sample=in_sample,
            period=period,
            filter_window=filter_window,
            seed=seed,
        )
        row["model"] = {
            "family": winner.get("family"),
            "k": winner.get("k"),
            "states": model.get("states"),
            "mapping": model.get("mapping"),
            "fitted_on": model.get("fitted_on"),
        }
        economic = economic_gate_fn(
            symbol=symbol,
            timeframe=timeframe,
            strategy=strategy,
            registry=registry,
            windows=tuple(economic_windows),
            gate_windows=tuple(gate_windows),
            surfaces=tuple(surfaces),
            model=model,
            period=period,
            capital=capital,
            platform=PLATFORM,
            direction=direction,
            params=params,
            thresholds=thresholds or {},
        )
        row["economic_report"] = economic
        row["pass"] = bool(economic.get("summary", {}).get("pass"))
        return row
    except Exception as exc:  # noqa: BLE001 - per-cell failure must be reported, not skipped.
        row["error"] = f"{type(exc).__name__}: {exc}"
        row["blocking_reasons"].append(row["error"])
        return row


def summarize(rows: list[dict], *, min_pass_cells: int) -> dict:
    passed = [r for r in rows if r.get("pass")]
    blocking = []
    if len(passed) < min_pass_cells:
        blocking.append(
            f"passed cells {len(passed)} < required {min_pass_cells}"
        )
    for row in rows:
        if row.get("pass"):
            continue
        prefix = row.get("dataset") or dataset_label(row.get("symbol", "?"),
                                                     row.get("timeframe", "?"))
        for reason in _cell_blocking_reasons(row):
            blocking.append(f"{prefix}: {reason}")
    return {
        "pass": not blocking,
        "passed_cells": len(passed),
        "total_cells": len(rows),
        "min_pass_cells": int(min_pass_cells),
        "blocking_reasons": blocking,
    }


def run_multi_asset_gate(
    *,
    datasets: Iterable[Dataset] = DATASETS,
    min_pass_cells: int = 3,
    in_sample: str = "is",
    held_out: str = "oos",
    bakeoff_windows: Iterable[str] = BAKEOFF_DEFAULT_WINDOWS,
    economic_windows: Iterable[str] = ECONOMIC_DEFAULT_WINDOWS,
    gate_windows: Iterable[str] = DEFAULT_GATE_WINDOWS,
    surfaces: Iterable[str] = SURFACES,
    families: Iterable[str] = ("hmm", "gmm", "kmeans"),
    k_range: Iterable[int] = range(2, 8),
    period: int = 48,
    filter_window: int = 64,
    seed: int = 0,
    strategy: str = DEFAULT_STRATEGY,
    registry: str = DEFAULT_REGISTRY,
    direction: str = DEFAULT_DIRECTION,
    params: Optional[dict] = None,
    capital: float = DEFAULT_CAPITAL,
    thresholds: Optional[dict] = None,
    bakeoff_fn: Callable[..., dict] = run_bakeoff,
    fit_model_fn: Callable[..., dict] = fit_selected_model,
    economic_gate_fn: Callable[..., dict] = run_gate,
) -> dict:
    datasets = list(datasets)
    rows = [
        run_cell(
            symbol,
            timeframe,
            in_sample=in_sample,
            held_out=held_out,
            bakeoff_windows=bakeoff_windows,
            economic_windows=economic_windows,
            gate_windows=gate_windows,
            surfaces=surfaces,
            families=families,
            k_range=k_range,
            period=period,
            filter_window=filter_window,
            seed=seed,
            strategy=strategy,
            registry=registry,
            direction=direction,
            params=params,
            capital=capital,
            thresholds=thresholds,
            bakeoff_fn=bakeoff_fn,
            fit_model_fn=fit_model_fn,
            economic_gate_fn=economic_gate_fn,
        )
        for symbol, timeframe in datasets
    ]
    return {
        "issue": 1083,
        "datasets": [{"symbol": s, "timeframe": tf} for s, tf in datasets],
        "in_sample": in_sample,
        "held_out": held_out,
        "bakeoff_windows": list(bakeoff_windows),
        "economic_windows": list(economic_windows),
        "gate_windows": list(gate_windows),
        "surfaces": list(surfaces),
        "families": list(families),
        "k_range": list(k_range),
        "period": int(period),
        "filter_window": int(filter_window),
        "seed": int(seed),
        "strategy": strategy,
        "registry": registry,
        "direction": direction,
        "platform": PLATFORM,
        "thresholds": thresholds or {},
        "rows": rows,
        "summary": summarize(rows, min_pass_cells=min_pass_cells),
    }


def format_report(report: dict) -> str:
    lines = []
    lines.append("=" * 96)
    lines.append(
        "REGIME MULTI-ASSET VOL/ECONOMIC GATE (#1083) "
        f"strategy={report['strategy']} registry={report['registry']}"
    )
    lines.append("Each row must clear #1080 model selection and #1081 ATR economics.")
    lines.append("=" * 96)
    hdr = f"{'dataset':16s} {'winner':14s} {'#1080':>6s} {'#1081':>6s} {'pass':>6s}  reason"
    lines.append(hdr)
    lines.append("-" * len(hdr))
    for row in report.get("rows", []):
        bakeoff = row.get("bakeoff") or {}
        winner = bakeoff.get("winner") or {}
        winner_label = (
            f"{winner.get('family')}:{winner.get('k')}"
            if winner else "-"
        )
        has_winner = bool(winner)
        econ_pass = bool((row.get("economic_report") or {}).get("summary", {}).get("pass"))
        reasons = "; ".join(_cell_blocking_reasons(row))
        lines.append(
            f"{row.get('dataset', '-')[:16]:16s} {winner_label[:14]:14s} "
            f"{'Y' if has_winner else '.':>6s} {'Y' if econ_pass else '.':>6s} "
            f"{'Y' if row.get('pass') else '.':>6s}  {reasons}"
        )
    lines.append("")
    summary = report.get("summary", {})
    lines.append("SUMMARY")
    lines.append(
        f"  pass: {bool(summary.get('pass'))} "
        f"({summary.get('passed_cells', 0)}/{summary.get('total_cells', 0)} cells; "
        f"required {summary.get('min_pass_cells', 0)})"
    )
    for reason in summary.get("blocking_reasons", []):
        lines.append(f"  block: {reason}")
    return "\n".join(lines)


def build_parser() -> argparse.ArgumentParser:
    p = argparse.ArgumentParser(description="#1083 multi-asset regime vol/economic gate")
    p.add_argument("--datasets", default="",
                   help="comma list SYMBOL:TIMEFRAME; default eval_windows.DATASETS")
    p.add_argument("--min-pass-cells", type=int, default=3)
    p.add_argument("--in-sample", default="is")
    p.add_argument("--held-out", default="oos")
    p.add_argument("--bakeoff-windows", default=",".join(BAKEOFF_DEFAULT_WINDOWS))
    p.add_argument("--economic-windows", default=",".join(ECONOMIC_DEFAULT_WINDOWS))
    p.add_argument("--gate-windows", default=",".join(DEFAULT_GATE_WINDOWS))
    p.add_argument("--surfaces", default=",".join(SURFACES),
                   help=f"comma list from {','.join(SURFACES)}")
    p.add_argument("--families", default="hmm,gmm,kmeans")
    p.add_argument("--k-range", default="2,3,4,5,6,7")
    p.add_argument("--period", type=int, default=48)
    p.add_argument("--filter-window", type=int, default=64)
    p.add_argument("--seed", type=int, default=0)
    p.add_argument("--strategy", default=DEFAULT_STRATEGY)
    p.add_argument("--registry", default=DEFAULT_REGISTRY, choices=["spot", "futures"])
    p.add_argument("--direction", default=DEFAULT_DIRECTION, choices=["long", "short", "both"])
    p.add_argument("--capital", type=float, default=DEFAULT_CAPITAL)
    p.add_argument("--params", default=None, help="JSON object of open-strategy params")
    p.add_argument("--json", default=None, help="write machine-readable report here")
    p.add_argument("--min-sharpe-delta", type=float, default=0.0)
    p.add_argument("--min-ddadj-delta", type=float, default=0.0)
    p.add_argument("--max-stop-rate-delta", type=float, default=0.0)
    p.add_argument("--min-mae-delta", type=float, default=0.0)
    return p


def main(argv=None) -> int:
    args = build_parser().parse_args(argv)
    params = json.loads(args.params) if args.params else None
    if params is not None and not isinstance(params, dict):
        raise SystemExit("--params must decode to a JSON object")
    datasets = parse_datasets(args.datasets)
    for window in [args.in_sample, args.held_out, *parse_csv(args.bakeoff_windows),
                   *parse_csv(args.economic_windows), *parse_csv(args.gate_windows)]:
        if window not in WINDOWS:
            raise SystemExit(f"unknown window {window}; known: {list(WINDOWS)}")
    surfaces = parse_csv(args.surfaces)
    bad_surfaces = sorted(set(surfaces) - set(SURFACES))
    if bad_surfaces:
        raise SystemExit(f"unknown surfaces {bad_surfaces}; known: {list(SURFACES)}")
    report = run_multi_asset_gate(
        datasets=datasets,
        min_pass_cells=args.min_pass_cells,
        in_sample=args.in_sample,
        held_out=args.held_out,
        bakeoff_windows=parse_csv(args.bakeoff_windows),
        economic_windows=parse_csv(args.economic_windows),
        gate_windows=parse_csv(args.gate_windows),
        surfaces=surfaces,
        families=parse_csv(args.families),
        k_range=parse_ints(args.k_range),
        period=args.period,
        filter_window=args.filter_window,
        seed=args.seed,
        strategy=args.strategy,
        registry=args.registry,
        direction=args.direction,
        params=params,
        capital=args.capital,
        thresholds={
            "min_sharpe_delta": args.min_sharpe_delta,
            "min_ddadj_delta": args.min_ddadj_delta,
            "max_stop_rate_delta": args.max_stop_rate_delta,
            "min_mae_delta": args.min_mae_delta,
        },
    )
    if args.json:
        with open(args.json, "w") as fh:
            json.dump(report, fh, indent=2, default=float)
            fh.write("\n")
    print(format_report(report))
    # When run interactively, include the detailed #1081 table for any failing cell
    # that reached the economic gate; this keeps the top matrix concise but actionable.
    for row in report.get("rows", []):
        econ = row.get("economic_report")
        if econ and not row.get("pass"):
            print()
            print(format_economic_report(econ))
    return 0 if report["summary"]["pass"] else 1


if __name__ == "__main__":
    raise SystemExit(main())

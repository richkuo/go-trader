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


def _row_symbol(row: dict) -> str:
    sym = row.get("symbol")
    if sym:
        return str(sym)
    dataset = str(row.get("dataset") or "")
    return dataset.split(" ", 1)[0] if dataset else "?"


# Mirrors the data-absence marker regime_1081_economic_gate sets on a window
# that has no cached data (it swallows the gap into a pass=False row rather than
# raising). If that wording ever drifts, the match below simply stops firing and
# the cell falls back to the prior ``fail`` classification — an audit-accuracy
# regression, never a verdict/safety change.
_ECONOMIC_NO_DATA_MARKER = "no cached data"


def _economic_failure_is_data_gap_only(economic_report: dict) -> bool:
    """True iff a non-passing #1081 report's gate-window blocks are ALL missing-
    data gaps — i.e. no candidate ever ran to a verdict on a data-bearing gate
    window.

    #1081 does not raise on a missing window; it returns a normal ``pass=False``
    report carrying a row with ``error='no cached data'``. Without this, the
    #1083 cell would read as a genuine economic ``fail`` when the data was simply
    absent. Safety constraint: if ANY gate window ran to a real (error-free)
    non-passing verdict, that is genuine negative evidence and the cell must stay
    ``fail`` even when another gate window is data-gapped. Degenerate-label or
    per-cell economic exceptions on data-bearing windows also stay ``fail`` (only
    an explicit data gap is inconclusive).
    """
    gate_windows = set(economic_report.get("gate_windows", []))
    non_passing = [
        r for r in economic_report.get("rows", [])
        if r.get("window") in gate_windows and not r.get("verdict", {}).get("pass")
    ]
    if not non_passing:
        return False
    # An error-free non-passing gate row is a real economic rejection.
    if any(not r.get("error") for r in non_passing):
        return False
    return any(_ECONOMIC_NO_DATA_MARKER in str(r.get("error", "")) for r in non_passing)


def _cell_outcome(row: dict) -> str:
    """Classify a cell as ``pass`` / ``fail`` / ``inconclusive``.

    - ``pass``: cleared both #1080 model selection and #1081 ATR economics.
    - ``fail``: the methodology ran on real data but did not clear — no #1080
      gate-passing model, or a genuine #1081 economic rejection (a candidate did
      not beat the flat control on a data-bearing gate window). Degenerate-label
      and per-cell economic exceptions on data-bearing windows also stay ``fail``.
      Genuine negative evidence about generalization on that cell.
    - ``inconclusive``: the cell could not be evaluated on real data — either a
      #1083-stage exception (in-sample data gap / loader-IO error, funneled into
      ``row['error']``), OR an #1081 economic failure whose gate-window blocks are
      ALL missing-data gaps (no candidate ran to a verdict). Absence of evidence,
      not evidence of absence: must NOT count as a failure (a data gap would
      otherwise masquerade as a model that does not generalize) and must NOT
      substitute for a pass.
    """
    if row.get("pass"):
        return "pass"
    if row.get("error"):
        return "inconclusive"
    economic = row.get("economic_report")
    if economic and _economic_failure_is_data_gap_only(economic):
        return "inconclusive"
    return "fail"


def summarize(rows: list[dict], *, min_pass_cells: int,
              min_pass_symbols: Optional[int] = None) -> dict:
    """Aggregate per-cell results into a structured breadth verdict.

    Promotion is a *generalization* claim, and the safe default is NOT promoting
    (live stays on flat ATR sizing), so the gate is fail-closed on two
    orthogonal breadth axes, BOTH of which must clear:

    1. **Cell breadth** — at least ``min_pass_cells`` cells pass. This tolerates
       a minority of genuine failures (one bad cell must not veto an otherwise
       broad pass — that would make the knob inert), but it is a hard floor:
       ``min_pass_cells > total`` blocks.
    2. **Cross-asset breadth** — the passing cells must span at least
       ``min_pass_symbols`` distinct symbols (default ``min(2, #symbols in the
       panel)``), so a model that merely racks up passes on one asset's several
       timeframes cannot be promoted as a *general* regime classifier. An
       explicit floor above the panel's symbol count blocks (unsatisfiable →
       fail-closed, mirroring the cell floor).

    ``inconclusive`` cells (per-cell exceptions / missing data) never count as a
    ``fail`` and never count as a ``pass`` — on the cell-breadth axis they neither
    help nor hurt. They are also never allowed to *lower* a breadth floor: the
    cross-asset floor is derived from the panel's full symbol roster
    (``min(2, #panel symbols)``), so a symbol whose cells are all inconclusive
    still counts toward ``required_symbols`` while contributing no passing symbol.
    This is deliberately fail-closed — a data gap must not make promotion easier.
    Consequence: on a 2-symbol panel where one symbol is wholly inconclusive,
    ``required_symbols == 2`` but at most one symbol can pass, so the gate blocks;
    on the default 3-symbol panel the floor caps at 2, so one wholly-inconclusive
    symbol still leaves promotion reachable.

    Per-non-pass-cell reasons are diagnostics (``cell_diagnostics``, tagged by
    outcome). They are promoted into ``blocking_reasons`` only when a breadth
    floor is missed, where they are the actionable cause.

    Raises ``ValueError`` if any breadth floor is non-positive: ``len(passed) >=
    k`` is vacuously true for ``k <= 0``, so a misconfigured floor (e.g. a wrapper
    computing ``len(datasets) - tolerance`` into negative territory) would
    green-light promoting a model that cleared no cell. The verdict must be
    reachable as ``True`` only when at least one genuine cell passed, so the floor
    is enforced at the decision boundary, not only at the CLI.
    """
    if min_pass_cells < 1:
        raise ValueError(f"min_pass_cells must be >= 1, got {min_pass_cells}")
    if min_pass_symbols is not None and min_pass_symbols < 1:
        raise ValueError(f"min_pass_symbols must be >= 1, got {min_pass_symbols}")
    passed = [r for r in rows if _cell_outcome(r) == "pass"]
    failed = [r for r in rows if _cell_outcome(r) == "fail"]
    inconclusive = [r for r in rows if _cell_outcome(r) == "inconclusive"]

    passing_symbols = sorted({_row_symbol(r) for r in passed})
    panel_symbols = sorted({_row_symbol(r) for r in rows})
    if min_pass_symbols is None:
        required_symbols = min(2, len(panel_symbols))
    else:
        required_symbols = int(min_pass_symbols)

    cell_diagnostics = []
    for row in rows:
        outcome = _cell_outcome(row)
        if outcome == "pass":
            continue
        prefix = row.get("dataset") or dataset_label(row.get("symbol", "?"),
                                                     row.get("timeframe", "?"))
        for reason in _cell_blocking_reasons(row):
            cell_diagnostics.append(f"[{outcome}] {prefix}: {reason}")

    breadth_met = len(passed) >= min_pass_cells
    symbols_met = len(passing_symbols) >= required_symbols
    blocking = []
    if not breadth_met:
        blocking.append(f"passed cells {len(passed)} < required {min_pass_cells}")
    if not symbols_met:
        shown = ",".join(passing_symbols) or "-"
        blocking.append(
            f"passing symbols {len(passing_symbols)} ({shown}) "
            f"< required {required_symbols}"
        )
    if blocking:
        blocking.extend(cell_diagnostics)

    return {
        "pass": breadth_met and symbols_met,
        "passed_cells": len(passed),
        "failed_cells": len(failed),
        "inconclusive_cells": len(inconclusive),
        "total_cells": len(rows),
        "min_pass_cells": int(min_pass_cells),
        "passing_symbols": passing_symbols,
        "panel_symbols": panel_symbols,
        "required_pass_symbols": int(required_symbols),
        "blocking_reasons": blocking,
        "cell_diagnostics": cell_diagnostics,
    }


def run_multi_asset_gate(
    *,
    datasets: Iterable[Dataset] = DATASETS,
    min_pass_cells: int = 3,
    min_pass_symbols: Optional[int] = None,
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
    # Materialize every swept iterable once: each is re-consumed per cell (via
    # tuple(...) in run_cell) and again for the report metadata below, so a
    # one-shot generator would empty after the first cell, silently starving
    # cells 2..N and blanking the report. datasets was already materialized.
    datasets = list(datasets)
    bakeoff_windows = list(bakeoff_windows)
    economic_windows = list(economic_windows)
    gate_windows = list(gate_windows)
    surfaces = list(surfaces)
    families = list(families)
    k_range = list(k_range)
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
        "summary": summarize(rows, min_pass_cells=min_pass_cells,
                             min_pass_symbols=min_pass_symbols),
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
        f"({summary.get('passed_cells', 0)}/{summary.get('total_cells', 0)} cells passed; "
        f"{summary.get('failed_cells', 0)} failed, "
        f"{summary.get('inconclusive_cells', 0)} inconclusive; "
        f"required {summary.get('min_pass_cells', 0)})"
    )
    passing_symbols = summary.get("passing_symbols", [])
    panel_symbols = summary.get("panel_symbols", [])
    lines.append(
        f"  symbols: {','.join(passing_symbols) or '-'} "
        f"({len(passing_symbols)}/{len(panel_symbols)} passed; "
        f"required {summary.get('required_pass_symbols', 0)})"
    )
    for reason in summary.get("blocking_reasons", []):
        lines.append(f"  block: {reason}")
    return "\n".join(lines)


def build_parser() -> argparse.ArgumentParser:
    p = argparse.ArgumentParser(description="#1083 multi-asset regime vol/economic gate")
    p.add_argument("--datasets", default="",
                   help="comma list SYMBOL:TIMEFRAME; default eval_windows.DATASETS")
    p.add_argument("--min-pass-cells", type=int, default=3)
    p.add_argument("--min-pass-symbols", type=int, default=None,
                   help="distinct passing symbols required for cross-asset "
                        "breadth; default min(2, #symbols in the panel)")
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
    bakeoff_windows = parse_csv(args.bakeoff_windows)
    economic_windows = parse_csv(args.economic_windows)
    gate_windows = parse_csv(args.gate_windows)
    surfaces = parse_csv(args.surfaces)
    families = parse_csv(args.families)
    k_range = parse_ints(args.k_range)
    for window in [args.in_sample, args.held_out, *bakeoff_windows,
                   *economic_windows, *gate_windows]:
        if window not in WINDOWS:
            raise SystemExit(f"unknown window {window}; known: {list(WINDOWS)}")
    bad_surfaces = sorted(set(surfaces) - set(SURFACES))
    if bad_surfaces:
        raise SystemExit(f"unknown surfaces {bad_surfaces}; known: {list(SURFACES)}")
    bad_families = sorted(set(families) - set(rvm.FITTERS))
    if bad_families:
        raise SystemExit(
            f"unknown families {bad_families}; known: {sorted(rvm.FITTERS)}"
        )
    # Every enumerated sweep input must be non-empty for any cell to be
    # evaluable; an empty list would otherwise burn per-cell data loads only to
    # mislabel the result "no #1080 gate-passing model" / "no gate-window rows".
    # Reject up front, mirroring parse_datasets' non-empty guard.
    for flag, values in (
        ("--families", families),
        ("--k-range", k_range),
        ("--surfaces", surfaces),
        ("--bakeoff-windows", bakeoff_windows),
        ("--economic-windows", economic_windows),
        ("--gate-windows", gate_windows),
    ):
        if not values:
            raise SystemExit(f"{flag} requires at least one value")
    if args.min_pass_cells < 1:
        raise SystemExit(f"--min-pass-cells must be >= 1, got {args.min_pass_cells}")
    if args.min_pass_symbols is not None and args.min_pass_symbols < 1:
        raise SystemExit(
            f"--min-pass-symbols must be >= 1, got {args.min_pass_symbols}"
        )
    report = run_multi_asset_gate(
        datasets=datasets,
        min_pass_cells=args.min_pass_cells,
        min_pass_symbols=args.min_pass_symbols,
        in_sample=args.in_sample,
        held_out=args.held_out,
        bakeoff_windows=bakeoff_windows,
        economic_windows=economic_windows,
        gate_windows=gate_windows,
        surfaces=surfaces,
        families=families,
        k_range=k_range,
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

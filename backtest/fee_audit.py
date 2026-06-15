#!/usr/bin/env python3
"""fee_audit.py — registry-wide trade-count x fee-drag selectivity screen (#999 M5).

The cheapest possible triage before any M1 effort is spent on a strategy: does
its edge survive its own churn? At the binanceus audit fee model (0.1% taker
per side, ~0.3% round-trip once spread is counted), a strategy that fires
350-800 times/year pays 100%+ of capital in fees, while passing incumbents
trade 0-8 times per window. This screen makes that arithmetic explicit and
reproducible.

For every registered strategy on the audit datasets/windows it runs each leg
TWICE on the eval_windows.py harness (#994):

  - net  leg: the audit-identical fee model (platform fee + 5 bps slippage);
  - gross leg: the SAME signals with commission AND slippage zeroed.

Signals are computed before the Backtester sees the frame and the long/flat
path is all-in/all-out, so the two runs produce identical trade counts — the
gross/net gap is pure friction drag, isolated per strategy.

The salvage test then sorts strategies into:

  - deprecate    — gross return is ALSO <= 0; no selectivity filter can save a
                   strategy with no positive edge to begin with.
  - graduate_m1  — gross > 0 but net <= 0; a real edge exists under the churn,
                   so the strategy graduates to an M1 application with "raise
                   selectivity" as the mechanism (#991/#992/#993 etc.).
  - healthy      — net > 0; edge already survives fees (the incumbent tier).
  - unscreened_short — short-capable (bidirectional / allow_short): the default
                   plain long/flat harness drops short entries, so a
                   non-positive long leg cannot justify deprecate/no_trades;
                   run again with --direction short to measure that leg. (A
                   positive long edge still graduates/passes, flagged
                   long-leg-only.)
  - no_trades    — never fired on the audit slices; unscored.

Output is ranked by fee drag and committed as a markdown table so the verdict
is reproducible.

All aggregation/ranking/rendering logic is pure (operates on plain dicts) and
unit-tested without data access — same architecture as eval_windows.py /
exit_diagnostics.py.

Usage:
  # full registry-wide screen, committed report
  uv run --no-sync python backtest/fee_audit.py --markdown docs/research/fee-audit-m5.md

  # a focused subset on one dataset/window (fast path)
  uv run --no-sync python backtest/fee_audit.py \
      --strategies vwap_reversion,macd,sma_crossover --windows oos --datasets BTC/USDT:4h

  # focused short-leg screen for a short-only strategy (#990)
  uv run --no-sync python backtest/fee_audit.py \
      --registry futures --strategies vwap_rejection_st --direction short
"""

from __future__ import annotations

import argparse
import json
import os
import statistics
import sys
from typing import List, Optional

_THIS_DIR = os.path.dirname(os.path.abspath(__file__))
if _THIS_DIR not in sys.path:
    sys.path.insert(0, _THIS_DIR)

from eval_windows import (  # noqa: E402  (path bootstrap above)
    DATASETS,
    DEFAULT_CAPITAL,
    PLATFORM,
    WINDOWS,
    dataset_key,
    parse_dataset_arg,
    run_leg,
)

# `hold` always emits signal=0 by design (never trades); screening it is noise.
SKIP_STRATEGIES = {"hold"}

# Strategies that emit signal=-1 as a SHORT ENTRY (not just a long exit). The
# plain long/flat backtest path drops short entries silently (backtester.py:
# 1712), so a `deprecate`/`no_trades` verdict on one of these would assert more
# than the harness measured — the short half is simply unscreened. This mirrors
# scheduler/init.go `bidirectionalPerpsStrategies` (the live source of truth);
# test_fee_audit::test_live_bidirectional_set_matches_go_source parses init.go
# and fails if the two drift, so the set cannot go silently stale. For the
# three names carrying an `allow_short` gate (mtf_confluence / vol_momentum /
# regime_adaptive — long-only spot variant, short-capable futures variant) the
# per-variant `allow_short` flag decides; the rest short unconditionally.
LIVE_BIDIRECTIONAL_STRATEGIES = frozenset({
    "triple_ema_bidir", "tema_cross_bd", "session_breakout", "donchian_breakout",
    "chart_pattern", "liquidity_sweeps", "bear_pullback_st", "vwap_rejection_st",
    "momentum_pro", "mean_reversion_pro", "consolidation_range", "mtf_confluence",
    "vol_momentum", "funding_skew", "regime_adaptive", "anchored_vwap",
})

# The #956/#963 protocol windows; held-out windows overlap `is` and would
# double-count trades, so the screen defaults to the disjoint protocol pair.
DEFAULT_WINDOWS = ("is", "oos")

YEAR_DAYS = 365.25

# `unscreened_short` sits between graduate and healthy in salvage interest: the
# strategy emitted short entries the plain long/flat harness silently dropped,
# so a `deprecate`/`no_trades` claim would assert more than was measured.
VERDICT_ORDER = ("deprecate", "graduate_m1", "healthy", "unscreened_short",
                 "no_trades")


# ---------------------------------------------------------------------------
# Pure helpers (unit-tested without data access).
# ---------------------------------------------------------------------------

def _mean(values: List[float]) -> Optional[float]:
    vals = [v for v in values if v is not None]
    return statistics.mean(vals) if vals else None


def strategy_is_short_capable(default_params: Optional[dict], name: str) -> bool:
    """Does this strategy variant open shorts the long/flat harness can't see?

    Short-capability is strategy metadata, NOT recoverable from the raw signal
    series (a long-only crossover and a bidirectional strategy both encode
    bearishness as ``signal == -1``). So we key off the live bidirectional set
    (LIVE_BIDIRECTIONAL_STRATEGIES, mirrored from init.go). A name carrying an
    ``allow_short`` flag is gated by that per-variant flag — the spot variant
    (allow_short False) is genuinely long-only and fully measured, the futures
    variant (allow_short True) is short-capable; a bidirectional name with no
    such flag shorts unconditionally.
    """
    if name not in LIVE_BIDIRECTIONAL_STRATEGIES:
        return False
    dp = default_params or {}
    if "allow_short" in dp:
        return bool(dp["allow_short"])
    return True


def trades_per_year(total_trades: int, total_span_days: float,
                    year_days: float = YEAR_DAYS) -> Optional[float]:
    """Annualized trade rate over the summed calendar span of the scored legs.

    Returns None when the span is missing/zero so a no-data strategy never
    divides by zero or reports a fabricated rate.
    """
    if not total_span_days or total_span_days <= 0:
        return None
    return total_trades / (total_span_days / year_days)


def salvage_verdict(total_trades: int, mean_gross: Optional[float],
                    mean_net: Optional[float],
                    short_unmeasured: bool = False) -> str:
    """The #999 salvage test (see module docstring).

    gross <= 0 dominates: a strategy with no positive zero-fee edge cannot be
    rescued by raising selectivity, so it is a deprecation candidate regardless
    of how negative its net return is.

    ``short_unmeasured`` guards the verdict's honesty: the plain long/flat
    harness silently drops short entries (eval_windows.count_dropped_short_
    entries), so for a strategy that wanted to short, a non-positive *measured*
    long leg cannot justify `deprecate`/`no_trades` — the short half is simply
    unknown. Such rows become `unscreened_short`. A positive long edge is a
    real finding even with the short half unmeasured, so `graduate_m1` /
    `healthy` still stand (the report flags them as long-leg-only).
    """
    if total_trades == 0 or mean_gross is None:
        return "unscreened_short" if short_unmeasured else "no_trades"
    if mean_gross <= 0:
        return "unscreened_short" if short_unmeasured else "deprecate"
    if mean_net is None or mean_net <= 0:
        return "graduate_m1"
    return "healthy"


def aggregate_strategy(strategy: str, registry_label: str,
                       leg_results: List[dict],
                       short_capable: bool = False) -> dict:
    """Collapse one strategy's per-leg net/gross results into a screen row.

    ``leg_results`` entries are dicts with either ``error`` set or the keys
    ``trades`` / ``span_days`` / ``net_ret`` / ``gross_ret`` / ``net_sharpe``.
    Errored or no-data legs are excluded from the aggregates (and counted).
    Means are taken per-leg (each window x dataset counts once), matching how
    eval_windows reports across the slices.
    """
    data_legs = [l for l in leg_results
                 if l.get("error") is None and l.get("net_ret") is not None]
    errors = [l for l in leg_results if l.get("error") is not None]
    n_legs = len(data_legs)

    total_trades = sum(int(l["trades"]) for l in data_legs)
    total_span = sum(float(l["span_days"]) for l in data_legs if l.get("span_days"))
    short_unmeasured = bool(short_capable)
    mean_gross = _mean([l.get("gross_ret") for l in data_legs])
    mean_net = _mean([l.get("net_ret") for l in data_legs])
    mean_sharpe = _mean([l.get("net_sharpe") for l in data_legs])
    n_liquidated = sum(1 for l in data_legs if l.get("liquidated"))

    fee_drag = (mean_gross - mean_net) if (mean_gross is not None
                                           and mean_net is not None) else None
    tpy = trades_per_year(total_trades, total_span)
    # fee_drag is a per-LEG mean (pp); total_trades sums across all legs, so a
    # unit-consistent per-trade rate must scale the numerator over the same set
    # of legs: total drag (mean x n_legs) / total trades. (#1003 review.)
    drag_per_trade = ((fee_drag * n_legs) / total_trades
                      if (fee_drag is not None and total_trades) else None)
    verdict = salvage_verdict(total_trades, mean_gross, mean_net, short_unmeasured)

    return {
        "strategy": strategy,
        "registry": registry_label,
        "trades": total_trades,
        "span_days": round(total_span, 2),
        "trades_per_year": round(tpy, 1) if tpy is not None else None,
        "mean_gross_ret": round(mean_gross, 3) if mean_gross is not None else None,
        "mean_net_ret": round(mean_net, 3) if mean_net is not None else None,
        "fee_drag_pp": round(fee_drag, 3) if fee_drag is not None else None,
        "drag_per_trade_pp": round(drag_per_trade, 4) if drag_per_trade is not None else None,
        "mean_net_sharpe": round(mean_sharpe, 3) if mean_sharpe is not None else None,
        "short_unmeasured": short_unmeasured,
        "n_legs": n_legs,
        "n_liquidated": n_liquidated,
        "n_errors": len(errors),
        "errors": errors,
        "verdict": verdict,
    }


def rank_rows(rows: List[dict]) -> List[dict]:
    """Sort by fee drag descending; no_trades rows last, then by name.

    A larger positive fee drag = a strategy bleeding more of its gross edge to
    churn, so it sits at the top of the screen where the salvage decision
    matters most.
    """
    def key(r):
        no_trades = r["verdict"] == "no_trades"
        drag = r["fee_drag_pp"] if r["fee_drag_pp"] is not None else float("-inf")
        # registry tiebreak so a name screened on both registries (#1003)
        # orders deterministically rather than by dict insertion.
        return (no_trades, -drag, r["strategy"], r.get("registry", ""))

    return sorted(rows, key=key)


def verdict_counts(rows: List[dict]) -> dict:
    counts = {v: 0 for v in VERDICT_ORDER}
    for r in rows:
        counts[r["verdict"]] = counts.get(r["verdict"], 0) + 1
    return counts


def _md_num(v, prec: int = 2) -> str:
    if v is None:
        return "—"
    return f"{v:.{prec}f}"


def render_markdown(ranked: List[dict], meta: dict) -> str:
    """Render the committed report table + footer sections."""
    lines = [
        "# Fee-aware selectivity audit (#999 M5)",
        "",
        "Registry-wide trade-count x fee-drag screen. Each strategy leg is run "
        "twice on the eval_windows.py harness — once with the audit fee model, "
        "once with commission and slippage zeroed — to isolate fee drag and "
        "apply the salvage test (does a positive *gross* edge exist under the "
        "churn?).",
        "",
        "## Reproduce",
        "",
        "```",
        meta["command"],
        "```",
        "",
        f"- Generated: {meta.get('date', 'see git history')}",
        f"- Registries: {meta['registry']}",
        f"- Windows: {meta['windows_desc']}",
        f"- Datasets: {meta['datasets_desc']}",
        f"- Direction: {meta.get('direction', 'long')}",
        f"- Capital: {meta['capital']}",
        f"- Fee model: {PLATFORM} platform taker fee + 5 bps slippage (net); "
        "commission=0 and slippage=0 (gross). Fee drag = mean per-leg "
        "(gross - net) return.",
        "",
        "Returns are mean per-leg total-return %; trades are summed across all "
        "scored legs; trades/yr is annualized over the summed calendar span. "
        "**Verdicts:** `deprecate` (gross <= 0, no edge to salvage), "
        "`graduate_m1` (gross > 0, net <= 0 — raise selectivity), `healthy` "
        "(net > 0), `unscreened_short` (emitted short entries the long/flat "
        "harness drops — long leg alone can't justify deprecate/no_trades), "
        "`no_trades` (never fired). A † flags a row whose short half was "
        "unmeasured (verdict reflects the long leg only).",
        "",
        "| rank | strategy | reg | trades | trades/yr | gross %/leg | net %/leg "
        "| fee drag (pp) | drag/trade (pp) | net Sharpe | verdict |",
        "|-----:|----------|-----|-------:|----------:|------------:|----------:"
        "|--------------:|----------------:|-----------:|---------|",
    ]
    for i, r in enumerate(ranked, 1):
        dagger = " †" if r.get("short_unmeasured") else ""
        lines.append(
            f"| {i} | {r['strategy']}{dagger} | {r['registry']} | {r['trades']} | "
            f"{_md_num(r['trades_per_year'], 1)} | {_md_num(r['mean_gross_ret'])} | "
            f"{_md_num(r['mean_net_ret'])} | {_md_num(r['fee_drag_pp'])} | "
            f"{_md_num(r['drag_per_trade_pp'], 4)} | {_md_num(r['mean_net_sharpe'])} | "
            f"`{r['verdict']}` |"
        )

    deprecate = [r for r in ranked if r["verdict"] == "deprecate"]
    graduate = [r for r in ranked if r["verdict"] == "graduate_m1"]
    unscreened = [r for r in ranked if r["verdict"] == "unscreened_short"]
    errored = [r for r in ranked if r["n_errors"]]

    lines += ["", "## Deprecation list (gross edge <= 0 — fee filter cannot save)", ""]
    if deprecate:
        for r in deprecate:
            lines.append(
                f"- **{r['strategy']}** ({r['registry']}): gross "
                f"{_md_num(r['mean_gross_ret'])}%, net {_md_num(r['mean_net_ret'])}%, "
                f"{r['trades']} trades ({_md_num(r['trades_per_year'], 1)}/yr)")
    else:
        lines.append("- (none)")

    lines += ["", "## M1 graduations (gross > 0, net <= 0 — mechanism: raise selectivity)", ""]
    if graduate:
        for r in graduate:
            note = " — long leg only (short half unscreened)" if r.get(
                "short_unmeasured") else " — raise selectivity"
            lines.append(
                f"- **{r['strategy']}** ({r['registry']}): gross "
                f"{_md_num(r['mean_gross_ret'])}%, net {_md_num(r['mean_net_ret'])}%, "
                f"fee drag {_md_num(r['fee_drag_pp'])}pp over {r['trades']} trades "
                f"({_md_num(r['trades_per_year'], 1)}/yr){note}")
    else:
        lines.append("- (none)")

    lines += ["", "## Unscreened short legs (long/flat harness drops short "
              "entries — verdict withheld)", ""]
    if unscreened:
        for r in unscreened:
            lines.append(
                f"- **{r['strategy']}** ({r['registry']}): short-capable "
                f"(bidirectional / allow_short); the long/flat harness measured "
                f"only its long leg (gross {_md_num(r['mean_gross_ret'])}%, net "
                f"{_md_num(r['mean_net_ret'])}% over {r['trades']} long trades). "
                f"Re-screen via the open/close engine (models both sides) "
                f"before any deprecate/graduate call.")
    else:
        lines.append("- (none)")

    liquidated = [r for r in ranked if r.get("n_liquidated")]
    if liquidated:
        lines += ["", "## Liquidated legs (equity hit 0 — metrics floored at "
                  "the bust bar, #1005)", ""]
        for r in liquidated:
            lines.append(
                f"- **{r['strategy']}** ({r['registry']}): "
                f"{r['n_liquidated']}/{r['n_legs']} leg(s) liquidated — "
                f"return/DD read −100% for those legs; means above include "
                f"the floored values")

    if errored:
        lines += ["", "## Errors / skips", ""]
        for r in errored:
            reasons = sorted({e.get("error", "?") for e in r["errors"]})
            lines.append(
                f"- **{r['strategy']}** ({r['registry']}): {r['n_errors']} "
                f"errored leg(s) — {'; '.join(reasons)}")

    return "\n".join(lines) + "\n"


# ---------------------------------------------------------------------------
# Leg execution (I/O; everything above stays pure).
# ---------------------------------------------------------------------------

def screen_leg(reg, name: str, symbol: str, timeframe: str,
               window: tuple, capital: float,
               direction: Optional[str] = None) -> Optional[dict]:
    """Run the net + gross legs for one (strategy, dataset, window).

    Returns None when the net leg has no data; an ``{"error": ...}`` dict when
    a leg raises; otherwise the paired net/gross metrics. One bad leg never
    aborts the registry-wide screen.
    """
    try:
        net = run_leg(reg, name, None, symbol, timeframe, window,
                      capital=capital, direction=direction)
    except Exception as exc:  # noqa: BLE001 — one strategy must not kill the run
        return {"dataset": dataset_key(symbol, timeframe), "error": f"net: {exc}"}
    if net is None:
        return None  # no data on this slice
    try:
        gross = run_leg(reg, name, None, symbol, timeframe, window,
                        capital=capital, direction=direction,
                        commission_pct=0.0, slippage_pct=0.0)
    except Exception as exc:  # noqa: BLE001
        return {"dataset": dataset_key(symbol, timeframe), "error": f"gross: {exc}"}
    if gross is None:
        return None
    # The net and gross runs must execute the IDENTICAL trade sequence for the
    # gap to be pure friction drag. A count mismatch (e.g. a cold-cache
    # fallback ran the legs on different data slices, or a future fill rule
    # turned out to be fee/slippage-sensitive) makes the drag meaningless —
    # demote to an error leg rather than report a garbage row. (#1003 review.)
    if int(net["trades"]) != int(gross["trades"]):
        return {
            "dataset": dataset_key(symbol, timeframe),
            "error": (f"net/gross trade-count mismatch "
                      f"({net['trades']} vs {gross['trades']}) — "
                      f"runs not comparable"),
        }
    return {
        "dataset": dataset_key(symbol, timeframe),
        "error": None,
        "trades": net["trades"],
        "span_days": net["span_days"],
        "net_ret": net["return_pct"],
        "gross_ret": gross["return_pct"],
        "net_sharpe": net["sharpe"],
        # #1005: either run blew the account (equity hit 0; metrics floored
        # at the bust bar) — surfaced so blown legs are never silent.
        "liquidated": bool(net.get("liquidated") or gross.get("liquidated")),
    }


def screen_strategy(reg, name: str, registry_label: str, datasets: List[tuple],
                    window_names: List[str], capital: float,
                    direction: Optional[str] = None) -> dict:
    """All legs for one strategy across the windows/datasets → aggregated row."""
    leg_results = []
    for wname in window_names:
        window = WINDOWS[wname]
        for symbol, timeframe in datasets:
            leg = screen_leg(reg, name, symbol, timeframe, window, capital,
                             direction=direction)
            if leg is None:
                continue
            leg["window"] = wname
            leg_results.append(leg)
    short_capable = (
        strategy_is_short_capable(_default_params(reg, name), name)
        and direction != "short"
    )
    return aggregate_strategy(name, registry_label, leg_results, short_capable)


def _default_params(reg, name) -> Optional[dict]:
    entry = reg.STRATEGY_REGISTRY.get(name) or {}
    return entry.get("default_params")


def enumerate_targets(registry_choice: str,
                      subset: Optional[List[str]] = None) -> List[tuple]:
    """Resolve (name, registry_label, reg_module) targets for the screen.

    For ``both`` a strategy present in spot is screened on the spot registry;
    futures-only names are appended on the futures registry. A shared name
    whose futures ``default_params`` differ materially from spot (e.g.
    ``momentum`` threshold 3.0 vs 5.0, ``allow_short`` flips) is a distinct
    configuration — the screen's whole subject is trade frequency — so it is
    screened on BOTH registries (two rows, distinct ``registry`` labels)
    rather than silently collapsed to the spot variant. Byte-identical shared
    names yield a single (spot) row. ``subset`` (the optional --strategies
    list) filters by name after resolution. (#1003 review.)
    """
    from registry_loader import load_registry

    targets: List[tuple] = []
    seen = set()
    spot_reg = None
    if registry_choice in ("spot", "both"):
        spot_reg = load_registry("spot")
        for n in spot_reg.list_strategies():
            if n in SKIP_STRATEGIES:
                continue
            targets.append((n, "spot", spot_reg))
            seen.add(n)
    if registry_choice in ("futures", "both"):
        fut_reg = load_registry("futures")
        for n in fut_reg.list_strategies():
            if n in SKIP_STRATEGIES:
                continue
            if registry_choice == "both" and n in seen:
                # Collapse only when the futures config is byte-identical to
                # spot; a materially different variant is screened on its own.
                if _default_params(spot_reg, n) == _default_params(fut_reg, n):
                    continue
            targets.append((n, "futures", fut_reg))

    if subset:
        want = {s.strip() for s in subset if s.strip()}
        targets = [t for t in targets if t[0] in want]
        missing = want - {t[0] for t in targets}
        if missing:
            raise SystemExit(
                f"unknown strategies for registry={registry_choice!r}: "
                f"{sorted(missing)}")
    return targets


def build_parser() -> argparse.ArgumentParser:
    p = argparse.ArgumentParser(
        description="Registry-wide trade-count x fee-drag selectivity screen (#999 M5)")
    p.add_argument("--registry", choices=["spot", "futures", "both"], default="both",
                   help="Registries to screen (default: both — futures-only names appended)")
    p.add_argument("--strategies", default=None,
                   help="Optional comma list to restrict the screen (e.g. M5 candidates)")
    p.add_argument("--windows", default=None,
                   help=f"Comma list of windows (default: {','.join(DEFAULT_WINDOWS)}). "
                        f"Known: {', '.join(WINDOWS)}")
    p.add_argument("--datasets", default=None,
                   help="Comma list of SYMBOL:TIMEFRAME (default: the six audit datasets)")
    p.add_argument("--direction", default=None, choices=["long", "short"],
                   help="Entry side to screen. Default is the historical "
                        "long/flat harness; pass short to measure a short leg "
                        "(signal=-1 opens, +1 closes) instead of withholding "
                        "short-capable rows as unscreened.")
    p.add_argument("--capital", type=float, default=DEFAULT_CAPITAL)
    p.add_argument("--json", default=None, dest="json_out",
                   help="Write the full structured result to this path")
    p.add_argument("--markdown", default=None, dest="markdown_out",
                   help="Write/overwrite the committed report table at this path")
    return p


def _resolve_windows(arg: Optional[str]) -> List[str]:
    if not arg:
        return list(DEFAULT_WINDOWS)
    names = [w.strip() for w in arg.split(",") if w.strip()]
    unknown = [w for w in names if w not in WINDOWS]
    if unknown:
        raise SystemExit(f"unknown windows {unknown}; known: {list(WINDOWS)}")
    return names


def main(argv: Optional[List[str]] = None) -> int:
    args = build_parser().parse_args(argv)

    window_names = _resolve_windows(args.windows)
    if args.datasets:
        datasets = [parse_dataset_arg(d) for d in args.datasets.split(",") if d.strip()]
    else:
        datasets = list(DATASETS)
    subset = args.strategies.split(",") if args.strategies else None

    targets = enumerate_targets(args.registry, subset)
    print(f"screening {len(targets)} strategies x {len(datasets)} datasets x "
          f"{len(window_names)} windows (net + gross runs each)")

    rows = []
    for idx, (name, label, reg) in enumerate(targets, 1):
        print(f"  [{idx}/{len(targets)}] {name} ({label}) ...", flush=True)
        rows.append(screen_strategy(reg, name, label, datasets, window_names,
                                    args.capital, direction=args.direction))

    ranked = rank_rows(rows)
    counts = verdict_counts(ranked)

    # Console summary.
    print(f"\n{'#':>3}  {'strategy':<22} {'reg':<7} {'trades':>7} {'tr/yr':>7} "
          f"{'gross%':>8} {'net%':>8} {'drag(pp)':>9}  verdict")
    for i, r in enumerate(ranked, 1):
        print(f"{i:>3}  {r['strategy']:<22} {r['registry']:<7} {r['trades']:>7} "
              f"{_md_num(r['trades_per_year'], 1):>7} {_md_num(r['mean_gross_ret']):>8} "
              f"{_md_num(r['mean_net_ret']):>8} {_md_num(r['fee_drag_pp']):>9}  "
              f"{r['verdict']}")
    print(f"\nverdicts: " + ", ".join(f"{v}={counts[v]}" for v in VERDICT_ORDER))

    from datetime import date
    meta = {
        "command": _reproduce_command(args),
        "date": date.today().isoformat(),
        "registry": args.registry,
        "windows_desc": ", ".join(
            f"{w} ({WINDOWS[w][0]} → {WINDOWS[w][1] or 'latest'})" for w in window_names),
        "datasets_desc": ", ".join(dataset_key(s, t) for s, t in datasets),
        "direction": args.direction or "long",
        "capital": args.capital,
    }

    if args.markdown_out:
        with open(args.markdown_out, "w") as fh:
            fh.write(render_markdown(ranked, meta))
        print(f"\nwrote {args.markdown_out}")

    if args.json_out:
        payload = {
            "registry": args.registry,
            "windows": {w: list(WINDOWS[w]) for w in window_names},
            "datasets": [dataset_key(s, t) for s, t in datasets],
            "direction": args.direction or "long",
            "capital": args.capital,
            "verdict_counts": counts,
            "rows": ranked,
        }
        with open(args.json_out, "w") as fh:
            json.dump(payload, fh, indent=2, default=str)
        print(f"wrote {args.json_out}")
    return 0


def _reproduce_command(args) -> str:
    parts = ["uv run --no-sync python backtest/fee_audit.py",
             f"--registry {args.registry}"]
    if args.strategies:
        parts.append(f"--strategies {args.strategies}")
    if args.windows:
        parts.append(f"--windows {args.windows}")
    if args.datasets:
        parts.append(f"--datasets {args.datasets}")
    if args.direction:
        parts.append(f"--direction {args.direction}")
    if args.capital != DEFAULT_CAPITAL:
        parts.append(f"--capital {args.capital}")
    if args.markdown_out:
        parts.append(f"--markdown {args.markdown_out}")
    return " ".join(parts)


if __name__ == "__main__":
    sys.exit(main())

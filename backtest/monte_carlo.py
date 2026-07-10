#!/usr/bin/env python3
"""monte_carlo.py — trade-order Monte Carlo resampler for drawdown and
risk-of-ruin distributions (#1274).

The validation stack reports path-dependent risk as single-path point
estimates: the Backtester's ``max_drawdown_pct`` is the drawdown of the ONE
realized trade ordering, and eval_windows/gross_edge_noise resample only the
per-trade MEAN. Max drawdown is an order statistic of the trade sequence —
the same multiset of trades in a different order produces a different max
drawdown — so a single historical ordering is one draw from a distribution.
This tool resamples the trade ORDER to produce that distribution:

  - P5 / P50 / P95 (configurable via --percentiles) of max drawdown and of
    final return across resampled equity paths;
  - P(final equity < starting equity) — probability the strategy ends
    underwater;
  - P(max drawdown >= kill-switch threshold) — probability of tripping the
    per-strategy drawdown limit. The threshold resolves from a live config
    (--config PATH --strategy-id ID, mirroring scheduler/config.go's
    hierarchy: explicit strategy max_drawdown_pct > platform risk override >
    type default 40/45/50/60) or from --kill-switch-pct (default 25, the
    portfolio-level kill-switch default in PortfolioRiskConfig).

Two resampling schemes, both run by default (--schemes to restrict):

  - ``permute`` — shuffle the observed trade order (identical multiset,
    order randomized); isolates pure sequencing risk.
  - ``block`` — CIRCULAR block bootstrap: sample contiguous blocks (with
    replacement, wrapping past the series end so tail trades are not
    under-sampled) and concatenate to the original length; preserves
    short-range autocorrelation in trade outcomes and also varies the trade
    multiset. Block length via --block-len (0 = auto, ceil(n^(1/3))).

Compounding model: per-trade percent returns are compounded multiplicatively
into an equity path marked at TRADE CLOSES only (equity *= 1 + r/100). This
matches the Backtester's single-mode full-equity deployment but is
trade-close resolution — intra-trade (bar-level) excursions are invisible, so
the resampled drawdowns UNDERSTATE what a bar-level equity curve would show.
Returns default to NET of fees (``pnl / (shares * entry_price)`` — Trade.pnl
has both commissions deducted; #1241 documents that ``pnl_pct`` is gross);
--returns gross switches to raw fill-price edge. A path whose equity touches
0 is a bust: it is sticky-floored there (max DD 100, final return -100),
mirroring the Backtester's #1005 liquidation-floor convention.

Both probabilities are add-one smoothed ((count + 1) / (n_paths + 1), the
gross_edge_noise convention) so a finite path count never reports exactly 0.
All statistics are stdlib-only and deterministic under --seed.

SUGGEST-ONLY / diagnostics-only: output never gates a promotion, writes a
config, or feeds a live path.

Three trade sources, exactly one per invocation: ``--trades-json`` (a saved
run), ``--strategy`` (one bare registry strategy on the M1 harness), or
``--candidate-json`` (a full eval_windows candidate — name, params, direction,
close_strategies, allowed_regimes, regime_windows_spec, stops — resampled
exactly as M1 scores it). The candidate source is what auto_suggest shells:
resampling a gate-carrying candidate as if it were the bare strategy would
report drawdowns for a strategy nobody is considering.

MULTI-LEG MODE (#1295): pass ``--windows`` and/or ``--datasets`` (comma lists)
instead of the singular ``--window``/``--dataset`` to fan one invocation across
every (window, dataset) pair, emitting one stats block per leg under a ``legs``
key — the same fan shape as the other M-harness payloads. ``--datasets``
defaults to the six audit datasets. The singular flags keep their original
single-leg payload shape (``observed`` + ``schemes`` at the top level).

Usage:
  # Resample the trades of a completed run saved as JSON (a results dict
  # with a "trades" list, or a bare list of trade dicts / percent returns)
  uv run --no-sync python backtest/monte_carlo.py --trades-json results.json

  # Run one leg on the M1 audit-identical harness and resample its trades
  uv run --no-sync python backtest/monte_carlo.py \\
      --strategy squeeze_momentum --dataset BTC/USDT:1h --window is

  # Resample a full candidate across the protocol windows and audit datasets
  uv run --no-sync python backtest/monte_carlo.py \\
      --candidate-json cand.json --windows is,oos

  # Kill-switch threshold from a live config's strategy entry
  uv run --no-sync python backtest/monte_carlo.py --trades-json results.json \\
      --config scheduler/config.json --strategy-id hl-btc-squeeze
"""

from __future__ import annotations

import argparse
import json
import math
import os
import sys
from random import Random
from typing import List, Optional, Sequence

_THIS_DIR = os.path.dirname(os.path.abspath(__file__))
if _THIS_DIR not in sys.path:
    sys.path.insert(0, _THIS_DIR)
sys.path.insert(0, os.path.join(_THIS_DIR, "..", "shared_tools"))

from exit_policy_ab import DEFAULT_SEED  # noqa: E402  (path bootstrap above)

DEFAULT_N_PATHS = 10000
DEFAULT_PERCENTILES = (5.0, 50.0, 95.0)
# Portfolio-level kill-switch default (scheduler/config.go
# PortfolioRiskConfig.MaxDrawdownPct).
DEFAULT_KILL_SWITCH_PCT = 25.0
SCHEMES = ("permute", "block")

# ---------------------------------------------------------------------------
# Mirror of scheduler/config.go loadConfig's per-strategy max_drawdown_pct
# hierarchy (strategy-specific > platform risk override > type default).
# Change only alongside the Go side.
# ---------------------------------------------------------------------------
GO_ID_PREFIX_PLATFORM = (
    ("ibkr-", "ibkr"),
    ("deribit-", "deribit"),
    ("hl-", "hyperliquid"),
    ("ts-", "topstep"),
    ("rh-", "robinhood"),
    ("luno-", "luno"),
    ("okx-", "okx"),
)
GO_TYPE_DEFAULT_MAX_DD = {"options": 40.0, "perps": 50.0, "futures": 45.0}
GO_FALLBACK_MAX_DD = 60.0


# ---------------------------------------------------------------------------
# Pure statistics (stdlib only; deterministic; unit-tested without data).
# ---------------------------------------------------------------------------

def equity_path_stats(returns_pct: Sequence[float]) -> tuple:
    """Compound per-trade percent returns into an equity path and score it.

    Returns ``(max_dd_pct, final_return_pct)`` where ``max_dd_pct`` is the
    max peak-to-trough drawdown as a POSITIVE magnitude (0 = never below a
    peak) so it compares directly against a kill-switch threshold, and
    ``final_return_pct`` is the compounded total return. Equity is marked at
    trade closes only (see module docstring). A path whose equity touches 0
    busts: sticky-floored per the #1005 convention (DD 100, final -100).
    Empty input → (0.0, 0.0).
    """
    equity = 1.0
    peak = 1.0
    max_dd = 0.0
    for r in returns_pct:
        equity *= 1.0 + r / 100.0
        if equity <= 0.0:
            return 100.0, -100.0
        if equity > peak:
            peak = equity
        dd = (peak - equity) / peak * 100.0
        if dd > max_dd:
            max_dd = dd
    return max_dd, (equity - 1.0) * 100.0


def percentile(sorted_values: Sequence[float], q: float) -> Optional[float]:
    """Linear-interpolation percentile (numpy's default) on a SORTED list.

    q in [0, 100]. None on an empty sample.
    """
    n = len(sorted_values)
    if n == 0:
        return None
    if n == 1:
        return float(sorted_values[0])
    pos = (q / 100.0) * (n - 1)
    lo = int(math.floor(pos))
    hi = min(lo + 1, n - 1)
    frac = pos - lo
    return float(sorted_values[lo] * (1.0 - frac) + sorted_values[hi] * frac)


def add_one_smoothed(count: int, n_paths: int) -> float:
    """(count + 1) / (n_paths + 1) — a finite path count never reports 0."""
    return (count + 1) / (n_paths + 1)


def auto_block_len(n: int) -> int:
    """Default block length ceil(n^(1/3)) — the standard n^(1/3) rate for
    block bootstraps, floored at 1."""
    return max(1, math.ceil(n ** (1.0 / 3.0)))


def permuted_path(values: Sequence[float], rng: Random) -> List[float]:
    """One shuffled copy of the observed trade order (same multiset)."""
    out = list(values)
    rng.shuffle(out)
    return out


def block_bootstrap_path(values: Sequence[float], block_len: int,
                         rng: Random) -> List[float]:
    """One circular-block-bootstrap path of the original length.

    Blocks of exactly ``block_len`` are drawn with replacement from random
    start offsets, wrapping past the series end (circular — tail trades are
    sampled as often as any other), concatenated, and truncated to len(values).
    """
    n = len(values)
    out: List[float] = []
    while len(out) < n:
        start = rng.randrange(n)
        for k in range(block_len):
            out.append(values[(start + k) % n])
            if len(out) == n:
                break
    return out


def resample_stats(values: Sequence[float], scheme: str,
                   n_paths: int = DEFAULT_N_PATHS,
                   block_len: int = 0,
                   seed: int = DEFAULT_SEED,
                   kill_switch_pct: float = DEFAULT_KILL_SWITCH_PCT,
                   percentiles: Sequence[float] = DEFAULT_PERCENTILES) -> dict:
    """Full Monte Carlo stats block for one resampling scheme.

    ``block_len`` <= 0 means auto (ceil(n^(1/3))); ignored for ``permute``.
    Empty input returns a degenerate block (n_trades 0, everything None) —
    there is no path to resample, and reporting a fabricated 0-risk number
    would be worse than reporting nothing.
    """
    if scheme not in SCHEMES:
        raise ValueError(f"unknown scheme {scheme!r}; known: {SCHEMES}")
    n = len(values)
    if n == 0:
        return {"scheme": scheme, "n_trades": 0, "n_paths": 0,
                "block_len": None, "kill_switch_pct": kill_switch_pct,
                "max_dd_pct_percentiles": None,
                "final_return_pct_percentiles": None,
                "p_final_below_start": None, "p_dd_ge_kill_switch": None}
    eff_block = None
    if scheme == "block":
        eff_block = block_len if block_len > 0 else auto_block_len(n)
    rng = Random(seed)
    dds: List[float] = []
    finals: List[float] = []
    for _ in range(n_paths):
        if scheme == "permute":
            path = permuted_path(values, rng)
        else:
            path = block_bootstrap_path(values, eff_block, rng)
        dd, fin = equity_path_stats(path)
        dds.append(dd)
        finals.append(fin)
    dds.sort()
    finals.sort()
    return {
        "scheme": scheme,
        "n_trades": n,
        "n_paths": n_paths,
        "block_len": eff_block,
        "kill_switch_pct": kill_switch_pct,
        "max_dd_pct_percentiles": {
            f"p{q:g}": round(percentile(dds, q), 4) for q in percentiles},
        "final_return_pct_percentiles": {
            f"p{q:g}": round(percentile(finals, q), 4) for q in percentiles},
        "p_final_below_start": round(
            add_one_smoothed(sum(1 for f in finals if f < 0.0), n_paths), 6),
        "p_dd_ge_kill_switch": round(
            add_one_smoothed(sum(1 for d in dds if d >= kill_switch_pct),
                             n_paths), 6),
    }


# ---------------------------------------------------------------------------
# Trade-series extraction (pure).
# ---------------------------------------------------------------------------

def trade_returns(trades: Sequence, returns: str = "net") -> List[float]:
    """Per-trade percent returns, in the recorded (chronological) order.

    Accepts Backtester ``Trade.to_dict()`` dicts or bare numbers. ``net``
    (default) computes ``pnl / (shares * entry_price) * 100`` — Trade.pnl has
    entry+exit commissions deducted (#1241) — falling back to the gross
    ``pnl_pct`` when notional is unavailable. ``gross`` reads ``pnl_pct``
    (fill-price edge, commissions excluded).
    """
    if returns not in ("net", "gross"):
        raise ValueError(f"returns must be 'net' or 'gross', got {returns!r}")
    out: List[float] = []
    for i, t in enumerate(trades):
        if isinstance(t, (int, float)):
            out.append(float(t))
            continue
        if returns == "gross":
            if "pnl_pct" not in t:
                raise ValueError(
                    f"trade {i} is missing 'pnl_pct', required for "
                    f"--returns gross")
            out.append(float(t["pnl_pct"]))
            continue
        shares = float(t.get("shares") or 0.0)
        entry = float(t.get("entry_price") or 0.0)
        notional = shares * entry
        if notional > 0 and t.get("pnl") is not None:
            out.append(float(t["pnl"]) / notional * 100.0)
        else:
            if "pnl_pct" not in t:
                raise ValueError(
                    f"trade {i} is missing 'pnl_pct' (required as a fallback "
                    f"when 'shares'/'entry_price'/'pnl' can't derive a net "
                    f"return)")
            out.append(float(t["pnl_pct"]))
    return out


def trades_from_json_payload(payload) -> List:
    """Extract the trade list from a results-JSON payload.

    Accepts a Backtester results dict (``{"trades": [...]}``) or a bare list
    of trade dicts / percent returns.
    """
    if isinstance(payload, dict):
        trades = payload.get("trades")
        if not isinstance(trades, list):
            raise ValueError(
                "results JSON is a dict without a 'trades' list; expected a "
                "Backtester results dict or a bare list of trades")
        return trades
    if isinstance(payload, list):
        return payload
    raise ValueError(f"unsupported results JSON payload type "
                     f"{type(payload).__name__}; expected dict or list")


def resolve_kill_switch_pct(cfg: dict, strategy_id: str) -> float:
    """Per-strategy drawdown threshold, mirroring scheduler/config.go's
    loadConfig hierarchy: explicit strategy ``max_drawdown_pct`` > platform
    ``risk.max_drawdown_pct`` override > type default (options 40, futures
    45, perps 50, else 60). Raises ValueError when the strategy is missing.
    """
    sc = None
    for cand in cfg.get("strategies") or []:
        if isinstance(cand, dict) and cand.get("id") == strategy_id:
            sc = cand
            break
    if sc is None:
        raise ValueError(f"strategy {strategy_id!r} not found in config")
    explicit = float(sc.get("max_drawdown_pct") or 0.0)
    if explicit > 0:
        return explicit
    platform = str(sc.get("platform") or "").strip()
    stype = str(sc.get("type") or "").strip()
    if not platform:
        sid = str(sc.get("id") or "")
        for prefix, name in GO_ID_PREFIX_PLATFORM:
            if sid.startswith(prefix):
                platform = name
                break
        else:
            platform = "deribit" if stype == "options" else "binanceus"
    platforms = cfg.get("platforms") or {}
    pc = platforms.get(platform) if isinstance(platforms, dict) else None
    if isinstance(pc, dict):
        risk = pc.get("risk") or {}
        pct = float(risk.get("max_drawdown_pct") or 0.0) \
            if isinstance(risk, dict) else 0.0
        if pct > 0:
            return pct
    return GO_TYPE_DEFAULT_MAX_DD.get(stype, GO_FALLBACK_MAX_DD)


# ---------------------------------------------------------------------------
# Leg execution (I/O; everything above stays pure).
# ---------------------------------------------------------------------------

def _leg_returns(leg: Optional[dict], returns: str) -> Optional[List[float]]:
    """Per-trade percent returns from a run_leg result; None when no data."""
    if leg is None:
        return None
    key = "pnl_pct_net" if returns == "net" else "pnl_pct"
    return [float(s[key]) for s in leg.get("trade_samples") or []]


def _load_reg_and_window(registry: str, window_name: str):
    from eval_windows import WINDOWS
    from registry_loader import load_registry

    if window_name not in WINDOWS:
        raise SystemExit(f"unknown window {window_name!r}; "
                         f"known: {list(WINDOWS)}")
    return load_registry(registry), WINDOWS[window_name]


def run_leg_trades(strategy: str, registry: str, params: Optional[dict],
                   dataset: str, window_name: str,
                   capital: float, direction: Optional[str],
                   returns: str) -> List[float]:
    """Run one (strategy, dataset, window) leg on the M1 audit-identical
    harness (eval_windows.run_leg, default friction) and return its per-trade
    percent returns in close order.

    The BARE-STRATEGY source: ``direction`` is threaded exactly as given (None
    = the engine's raw-signal default), matching gross_edge_noise's leg. A full
    candidate goes through ``run_candidate_leg_trades`` instead, where the
    validated "long" default applies (#996).
    """
    from eval_windows import parse_dataset_arg, run_leg

    reg, window = _load_reg_and_window(registry, window_name)
    try:
        symbol, timeframe = parse_dataset_arg(dataset)
    except ValueError:
        raise SystemExit(f"--dataset expects SYMBOL:TIMEFRAME, got: {dataset!r}")
    if strategy not in reg.STRATEGY_REGISTRY:
        raise SystemExit(f"Unknown strategy {strategy!r}; available: "
                         f"{reg.list_strategies()}")
    values = _leg_returns(
        run_leg(reg, strategy, params, symbol, timeframe, window,
                capital=capital, direction=direction, keep_trades=True),
        returns)
    if values is None:
        raise SystemExit(f"no cached data for {dataset} in window "
                         f"{window_name!r}")
    return values


def run_candidate_leg_trades(candidate: dict, registry: str, dataset: str,
                             window_name: str, capital: float,
                             returns: str) -> Optional[List[float]]:
    """Per-trade returns for ONE candidate leg, resampled exactly as M1 scores
    it — the full candidate shape (close stack, entry gate, stops, profile
    allocation) is threaded via ``eval_windows.run_candidate_leg``, never a
    hand-picked subset. ``None`` when the dataset has no cached bars in the
    window (a missing dataset must not abort a multi-leg fan)."""
    from eval_windows import parse_dataset_arg, run_candidate_leg

    reg, window = _load_reg_and_window(registry, window_name)
    try:
        symbol, timeframe = parse_dataset_arg(dataset)
    except ValueError:
        raise SystemExit(f"--datasets expects SYMBOL:TIMEFRAME, got: {dataset!r}")
    if candidate["name"] not in reg.STRATEGY_REGISTRY:
        raise SystemExit(f"Unknown strategy {candidate['name']!r}; available: "
                         f"{reg.list_strategies()}")
    return _leg_returns(
        run_candidate_leg(reg, candidate, symbol, timeframe, window,
                          capital=capital, keep_trades=True),
        returns)


def default_dataset_args() -> List[str]:
    """The six audit datasets as SYMBOL:TIMEFRAME strings (eval_windows SSoT)."""
    from eval_windows import DATASETS
    return [f"{sym}:{tf}" for sym, tf in DATASETS]


def leg_blocks(values: Sequence[float], schemes: Sequence[str], *,
               n_paths: int, block_len: int, seed: int,
               kill_switch_pct: float,
               percentiles: Sequence[float]) -> List[dict]:
    """One resample_stats block per scheme, for a single leg's trade series."""
    return [resample_stats(values, scheme, n_paths=n_paths,
                           block_len=block_len, seed=seed,
                           kill_switch_pct=kill_switch_pct,
                           percentiles=percentiles)
            for scheme in schemes]


# ---------------------------------------------------------------------------
# Reporting / CLI.
# ---------------------------------------------------------------------------

def _fmt(v, prec=2):
    return "-" if v is None else f"{v:+.{prec}f}"


def format_report(source: str, returns: str, observed: tuple,
                  threshold_source: str, blocks: List[dict]) -> str:
    obs_dd, obs_final = observed
    lines = [
        f"trade-order Monte Carlo: {source} ({returns} per-trade returns)",
        f"observed single path: max DD {obs_dd:.2f}%, "
        f"final return {_fmt(obs_final)}%",
        f"kill-switch threshold: "
        f"{blocks[0]['kill_switch_pct']:g}% ({threshold_source})",
    ]
    for b in blocks:
        lines.append("")
        if b["n_trades"] == 0:
            lines.append(f"{b['scheme']}: no trades — nothing to resample")
            continue
        head = (f"{b['scheme']}: {b['n_paths']} paths over "
                f"{b['n_trades']} trades")
        if b["block_len"]:
            head += f", block_len={b['block_len']}"
        lines.append(head)
        dd = b["max_dd_pct_percentiles"]
        fr = b["final_return_pct_percentiles"]
        pct_keys = list(dd)
        lines.append("  max drawdown %:  " + "  ".join(
            f"{k}={dd[k]:.2f}" for k in pct_keys))
        lines.append("  final return %:  " + "  ".join(
            f"{k}={_fmt(fr[k])}" for k in pct_keys))
        lines.append(f"  P(final < start) = {b['p_final_below_start']:.4f}   "
                     f"P(max DD >= {b['kill_switch_pct']:g}%) = "
                     f"{b['p_dd_ge_kill_switch']:.4f}   (add-one smoothed)")
    return "\n".join(lines)


def format_multileg_report(source: str, returns: str, threshold_source: str,
                           kill_switch_pct: float, legs: List[dict]) -> str:
    lines = [
        f"trade-order Monte Carlo: {source} ({returns} per-trade returns)",
        f"kill-switch threshold: {kill_switch_pct:g}% ({threshold_source})",
        f"{len(legs)} leg(s)",
    ]
    for leg in legs:
        lines.append("")
        head = f"-- {leg['window']} / {leg['dataset']}"
        if leg["status"] != "ok":
            lines.append(f"{head}: {leg['status']} — nothing to resample")
            continue
        obs = leg["observed"]
        lines.append(f"{head} ({leg['n_trades']} trades)")
        lines.append(f"  observed single path: max DD "
                     f"{obs['max_dd_pct']:.2f}%, final return "
                     f"{_fmt(obs['final_return_pct'])}%")
        for b in leg["schemes"]:
            if b["n_trades"] == 0:
                lines.append(f"  {b['scheme']}: no trades")
                continue
            dd = b["max_dd_pct_percentiles"]
            fr = b["final_return_pct_percentiles"]
            keys = list(dd)
            lines.append(f"  {b['scheme']}: maxDD% "
                         + "  ".join(f"{k}={dd[k]:.2f}" for k in keys)
                         + " | final% "
                         + "  ".join(f"{k}={_fmt(fr[k])}" for k in keys))
            lines.append(f"    P(final < start) = "
                         f"{b['p_final_below_start']:.4f}   "
                         f"P(max DD >= {b['kill_switch_pct']:g}%) = "
                         f"{b['p_dd_ge_kill_switch']:.4f}   (add-one smoothed)")
    return "\n".join(lines)


def build_parser() -> argparse.ArgumentParser:
    p = argparse.ArgumentParser(
        description="Trade-order Monte Carlo resampler — drawdown / "
                    "risk-of-ruin distributions (#1274). Suggest-only.")
    src = p.add_argument_group("trade source (pick one)")
    src.add_argument("--trades-json", default=None,
                     help="Results JSON: a Backtester results dict with a "
                          "'trades' list, or a bare list of trade dicts / "
                          "percent returns")
    src.add_argument("--strategy", default=None,
                     help="Run one leg in-process on the M1 audit harness "
                          "(registry default params unless --params)")
    src.add_argument("--candidate-json", default=None,
                     help="Path to a full eval_windows candidate JSON (name, "
                          "params, direction, close_strategies, "
                          "allowed_regimes, stops, ...). Resampled exactly as "
                          "M1 scores it")
    p.add_argument("--registry", choices=["spot", "futures"], default="spot")
    p.add_argument("--params", default=None,
                   help="Params JSON for --strategy (default: registry "
                        "default_params)")
    p.add_argument("--dataset", default=None,
                   help="SYMBOL:TIMEFRAME single-leg source (default "
                        "BTC/USDT:1h); mutually exclusive with --datasets")
    p.add_argument("--window", default=None,
                   help="eval_windows window name, single-leg "
                        "(default: is); mutually exclusive with --windows")
    p.add_argument("--windows", default=None,
                   help="Comma list of eval_windows window names — multi-leg "
                        "mode (#1295); one stats block per (window, dataset)")
    p.add_argument("--datasets", default=None,
                   help="Comma list of SYMBOL:TIMEFRAME — multi-leg mode "
                        "(#1295). Default in multi-leg: the six audit datasets")
    p.add_argument("--direction", default=None, choices=["long", "short"])
    p.add_argument("--capital", type=float, default=1000.0)
    p.add_argument("--returns", choices=["net", "gross"], default="net",
                   help="Per-trade return basis (default net — fees "
                        "deducted; see module docstring)")
    p.add_argument("--schemes", default=",".join(SCHEMES),
                   help=f"Comma list of resampling schemes "
                        f"(default: {','.join(SCHEMES)})")
    p.add_argument("--n-paths", type=int, default=DEFAULT_N_PATHS)
    p.add_argument("--block-len", type=int, default=0,
                   help="Block length for the block scheme "
                        "(0 = auto, ceil(n^(1/3)))")
    p.add_argument("--percentiles", default=",".join(
        f"{q:g}" for q in DEFAULT_PERCENTILES))
    p.add_argument("--seed", type=int, default=DEFAULT_SEED)
    thr = p.add_argument_group("kill-switch threshold")
    thr.add_argument("--kill-switch-pct", type=float, default=None,
                     help=f"Drawdown threshold %% (default "
                          f"{DEFAULT_KILL_SWITCH_PCT:g} — the portfolio "
                          f"kill-switch default — unless --config resolves "
                          f"a per-strategy value)")
    thr.add_argument("--config", default=None,
                     help="Live go-trader config to resolve the per-strategy "
                          "threshold from (requires --strategy-id)")
    thr.add_argument("--strategy-id", default=None,
                     help="Strategy id inside --config whose max_drawdown_pct "
                          "hierarchy sets the threshold")
    p.add_argument("--json", default=None, dest="json_out",
                   help="Write the full structured result to this path")
    return p


def main(argv: Optional[List[str]] = None) -> int:
    args = build_parser().parse_args(argv)

    sources = [bool(args.trades_json), bool(args.strategy),
               bool(args.candidate_json)]
    if sum(sources) != 1:
        raise SystemExit("pass exactly one trade source: --trades-json, "
                         "--strategy or --candidate-json")

    if args.candidate_json and (args.params or args.direction):
        raise SystemExit("--params/--direction are --strategy flags; a "
                         "candidate JSON carries its own params and direction")

    multileg = bool(args.windows) or bool(args.datasets)
    if multileg:
        if args.trades_json:
            raise SystemExit("--windows/--datasets are leg-fan flags; they do "
                             "not apply to --trades-json (a saved run is one "
                             "already-realized leg)")
        if args.window or args.dataset:
            raise SystemExit("--windows/--datasets (multi-leg) are mutually "
                             "exclusive with --window/--dataset (single-leg)")

    if bool(args.config) != bool(args.strategy_id):
        raise SystemExit("--config and --strategy-id go together")
    if args.kill_switch_pct is not None and args.config:
        raise SystemExit("--kill-switch-pct and --config are mutually "
                         "exclusive threshold sources")

    schemes = [s.strip() for s in args.schemes.split(",") if s.strip()]
    if not schemes:
        raise SystemExit(f"--schemes must name at least one scheme; "
                         f"known: {list(SCHEMES)}")
    unknown = [s for s in schemes if s not in SCHEMES]
    if unknown:
        raise SystemExit(f"unknown schemes {unknown}; known: {list(SCHEMES)}")

    if args.n_paths < 1:
        raise SystemExit(f"--n-paths must be >= 1, got {args.n_paths}")

    try:
        percentiles = [float(q) for q in args.percentiles.split(",") if q.strip()]
    except ValueError:
        raise SystemExit(f"--percentiles values must be numeric, got "
                         f"{args.percentiles!r}")
    if not percentiles:
        raise SystemExit("--percentiles must name at least one value in "
                         "[0, 100]")
    out_of_range = [q for q in percentiles if q < 0.0 or q > 100.0]
    if out_of_range:
        raise SystemExit(f"--percentiles values must be in [0, 100], got "
                         f"{out_of_range}")

    if args.config:
        try:
            with open(args.config) as fh:
                cfg = json.load(fh)
        except OSError as exc:
            raise SystemExit(f"--config {args.config!r} could not be read: {exc}")
        except json.JSONDecodeError as exc:
            raise SystemExit(f"--config {args.config!r} is not valid JSON: {exc}")
        try:
            kill_switch = resolve_kill_switch_pct(cfg, args.strategy_id)
        except ValueError as exc:
            raise SystemExit(str(exc))
        threshold_source = (f"--config {args.config} strategy "
                            f"{args.strategy_id}")
    elif args.kill_switch_pct is not None:
        kill_switch = args.kill_switch_pct
        threshold_source = "--kill-switch-pct"
    else:
        kill_switch = DEFAULT_KILL_SWITCH_PCT
        threshold_source = "default (portfolio kill switch)"

    candidate = None
    if args.candidate_json:
        from eval_windows import validate_candidate
        try:
            with open(args.candidate_json) as fh:
                candidate = json.load(fh)
        except OSError as exc:
            raise SystemExit(f"--candidate-json {args.candidate_json!r} could "
                             f"not be read: {exc}")
        except json.JSONDecodeError as exc:
            raise SystemExit(f"--candidate-json {args.candidate_json!r} is not "
                             f"valid JSON: {exc}")
        try:
            validate_candidate(candidate)
        except ValueError as exc:
            raise SystemExit(f"--candidate-json is not a valid candidate: {exc}")

    common = dict(n_paths=args.n_paths, block_len=args.block_len,
                  seed=args.seed, kill_switch_pct=kill_switch,
                  percentiles=percentiles)

    if multileg:
        from eval_windows import dataset_key, parse_dataset_arg, run_leg

        window_names = ([w.strip() for w in args.windows.split(",") if w.strip()]
                        if args.windows else ["is", "oos"])
        dataset_args = ([d.strip() for d in args.datasets.split(",") if d.strip()]
                        if args.datasets else default_dataset_args())
        if not window_names or not dataset_args:
            raise SystemExit("--windows/--datasets must each name at least "
                             "one value")
        try:
            params = json.loads(args.params) if args.params else None
        except json.JSONDecodeError as exc:
            raise SystemExit(f"--params must be valid JSON: {exc}")

        legs = []
        for wname in window_names:
            for ds in dataset_args:
                try:
                    symbol, timeframe = parse_dataset_arg(ds)
                except ValueError:
                    raise SystemExit(f"--datasets expects SYMBOL:TIMEFRAME, "
                                     f"got: {ds!r}")
                if candidate is not None:
                    values = run_candidate_leg_trades(
                        candidate, args.registry, ds, wname, args.capital,
                        args.returns)
                else:
                    # Bare strategy: the gross_edge_noise leg (direction
                    # threaded as given), fanned across windows/datasets.
                    reg, window = _load_reg_and_window(args.registry, wname)
                    if args.strategy not in reg.STRATEGY_REGISTRY:
                        raise SystemExit(
                            f"Unknown strategy {args.strategy!r}; available: "
                            f"{reg.list_strategies()}")
                    values = _leg_returns(
                        run_leg(reg, args.strategy, params, symbol, timeframe,
                                window, capital=args.capital,
                                direction=args.direction, keep_trades=True),
                        args.returns)
                leg = {"window": wname, "dataset": dataset_key(symbol, timeframe)}
                if values is None:
                    # A dataset with no cached bars in this window must not
                    # abort the fan — record it and keep going.
                    leg.update({"status": "no_data", "n_trades": 0,
                                "observed": None, "schemes": []})
                else:
                    obs = equity_path_stats(values)
                    leg.update({
                        "status": "ok", "n_trades": len(values),
                        "observed": {"max_dd_pct": round(obs[0], 4),
                                     "final_return_pct": round(obs[1], 4)},
                        "schemes": leg_blocks(values, schemes, **common)})
                legs.append(leg)

        name = candidate["name"] if candidate is not None else args.strategy
        source = (f"{name} windows={','.join(window_names)} "
                  f"({args.registry} registry)")
        print(format_multileg_report(source, args.returns, threshold_source,
                                     kill_switch, legs))

        if not any(leg["status"] == "ok" for leg in legs):
            sys.stderr.write("no cached data for any (window, dataset) leg\n")
            return 1

        if args.json_out:
            payload = {
                "source": source, "returns": args.returns, "seed": args.seed,
                "n_paths": args.n_paths, "percentiles": percentiles,
                "kill_switch_pct": kill_switch,
                "kill_switch_source": threshold_source,
                "candidate": candidate,
                "legs": legs,
            }
            with open(args.json_out, "w") as fh:
                json.dump(payload, fh, indent=2, default=str)
            print(f"\nwrote {args.json_out}")
        return 0

    window = args.window or "is"
    dataset = args.dataset or "BTC/USDT:1h"

    if args.trades_json:
        try:
            with open(args.trades_json) as fh:
                payload = json.load(fh)
        except OSError as exc:
            raise SystemExit(
                f"--trades-json {args.trades_json!r} could not be read: {exc}")
        except json.JSONDecodeError as exc:
            raise SystemExit(
                f"--trades-json {args.trades_json!r} is not valid JSON: {exc}")
        try:
            trades = trades_from_json_payload(payload)
            values = trade_returns(trades, returns=args.returns)
        except ValueError as exc:
            raise SystemExit(str(exc))
        source = args.trades_json
    elif candidate is not None:
        values = run_candidate_leg_trades(candidate, args.registry, dataset,
                                          window, args.capital, args.returns)
        if values is None:
            raise SystemExit(f"no cached data for {dataset} in window "
                             f"{window!r}")
        source = (f"{candidate['name']} {dataset} window={window} "
                  f"({args.registry} registry, candidate-json)")
    else:
        try:
            params = json.loads(args.params) if args.params else None
        except json.JSONDecodeError as exc:
            raise SystemExit(f"--params must be valid JSON: {exc}")
        values = run_leg_trades(args.strategy, args.registry, params,
                                dataset, window, args.capital,
                                args.direction, args.returns)
        source = (f"{args.strategy} {dataset} window={window} "
                  f"({args.registry} registry)")

    observed = equity_path_stats(values)
    blocks = leg_blocks(values, schemes, **common)

    print(format_report(source, args.returns, observed, threshold_source,
                        blocks))

    if args.json_out:
        payload = {
            "source": source,
            "returns": args.returns,
            "n_trades": len(values),
            "seed": args.seed,
            "n_paths": args.n_paths,
            "percentiles": percentiles,
            "kill_switch_pct": kill_switch,
            "kill_switch_source": threshold_source,
            "observed": {"max_dd_pct": round(observed[0], 4),
                         "final_return_pct": round(observed[1], 4)},
            "schemes": blocks,
        }
        with open(args.json_out, "w") as fh:
            json.dump(payload, fh, indent=2, default=str)
        print(f"\nwrote {args.json_out}")
    return 0


if __name__ == "__main__":
    sys.exit(main())

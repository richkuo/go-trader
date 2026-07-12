#!/usr/bin/env python3
"""#1338: One-command tuning pipeline for LIVE strategies.

Every piece needed to retune a deployed strategy already exists — walk-forward
parameter search (``optimizer.walk_forward_optimize``), the M-harness promotion
gate under one Benjamini-Hochberg correction (``auto_suggest``), and a config
loader that resolves a strategy's exact live effective parameters
(``run_backtest.load_strategy_config``). Nothing connected them, so systematic
tuning of the live fleet never happened and deployed parameters went stale.

This driver connects them. For each (or one named) strategy in a live config it:

  1. Resolves the exact live baseline — open name + effective open params +
     effective close stack + direction + regime gate — by REUSING
     ``load_strategy_config`` (never reimplementing config resolution). The live
     baseline is always candidate zero, the incumbent every survivor must beat.
  2. Generates a bounded candidate neighborhood around each tunable open param
     (registry ``constraints`` respected), unioned with the strategy's
     ``DEFAULT_PARAM_RANGES`` row; operator ``--param`` / ``--overrides`` replace
     or freeze specific params, validated fail-loud.
  3. Stage 1 — walk-forward search over the neighborhood, pruned to OOS-stable
     survivors. Short-direction strategies skip stage 1 (the warmup seeder is
     long-only) and send the full neighborhood to stage 2; bidirectional
     strategies DO run stage 1 with ``direction="both"``.
  4. Stage 2 — the survivors go through the M-harness gate via ``auto_suggest``
     under a SELECTION-AWARE correction: (a) stage-1 search data is sliced to end
     strictly before the earliest stage-2 evidence window (disjoint evidence),
     and (b) the BH family size is the searched count N, not the survivor count
     (``correction.family_size``, the #1338 auto_suggest extension).
  5. Emits a versioned, machine-consumable artifact (ranked verdicts, per-survivor
     config patches, the full recorded baseline) plus a progress JSON — the
     #1339-#1341 dashboard consumes these, so the layout is stable and schema-
     versioned.

SUGGEST-ONLY — HARD BOUNDARY (same invariant as #1210). This tool NEVER writes a
live config, a default, or a PR, and never shells git/gh. It emits a ranked
report; a human makes every promotion call.

Usage:
  uv run --no-sync python backtest/tune_live.py --config <cfg> [--strategy <id>] \\
      [--registry spot|futures] [--datasets BTC/USDT:1h] [--windows is,oos] \\
      [--param fast_period=10,15,20] [--overrides overrides.json] \\
      [--out-dir DIR] [--json OUT.json] [--jobs 4] [--dry-run]
"""
from __future__ import annotations

import argparse
import copy
import itertools
import json
import os
import sys

_THIS_DIR = os.path.dirname(os.path.abspath(__file__))
_REPO = os.path.abspath(os.path.join(_THIS_DIR, ".."))
for _p in (_THIS_DIR, os.path.join(_REPO, "shared_tools")):
    if _p not in sys.path:
        sys.path.insert(0, _p)

import auto_suggest  # noqa: E402
from data_fetcher import load_cached_data  # noqa: E402 (module-level: tests patch tune_live.load_cached_data)
from eval_windows import WINDOWS as M1_WINDOWS, PLATFORM as DATA_PLATFORM, FEE_PLATFORM  # noqa: E402
from optimizer import DEFAULT_PARAM_RANGES, generate_param_grid, walk_forward_optimize  # noqa: E402
from registry_loader import load_registry  # noqa: E402
from run_backtest import (  # noqa: E402
    FUNDING_COLUMN_STRATEGIES,
    _attach_funding_if_needed,
    load_strategy_config,
)

# Downstream consumers (#1339-#1341) read this schema; bump on any layout change.
SCHEMA_VERSION = 1
ISSUE = 1338

DEFAULT_SINCE = "2019-01-01"          # stage-1 history start (sliced to end before stage 2)
DEFAULT_STAGE2_WINDOWS = ["is", "oos"]  # M1 audit protocol windows (eval_windows.WINDOWS)
DEFAULT_ALPHA = 0.05
DEFAULT_SPLITS = 5
DEFAULT_CAPITAL = 1000.0
DEFAULT_METRIC = "sharpe_ratio"
# Stage-1 slice must leave at least this many bars, else walk-forward cannot form
# even one usable fold (window_size >= 50 per split) — refuse loudly instead.
MIN_STAGE1_BARS = 50 * DEFAULT_SPLITS
# Cap on stage-2 CANDIDATE count (not the searched family N). Guards the
# stage-1-skipped path (short / composite regime), where the whole neighborhood
# becomes candidates — a silent 200-M1-run storm reads as "covered everything".
# Refuse loudly and tell the operator to narrow with --param/--overrides.
DEFAULT_MAX_CANDIDATES = 64

STAGE2_HARNESSES = ["m1_noise", "m1", "m3", "m5"]  # "mc" opt-in only (#1316), advisory (#1295)

FOOTER = ("Suggest-only. tune_live never wrote a config, a live default, or a "
          "PR — every ranked patch is a proposal a human must promote.")

# Live resolution fields the backtest candidate / walk-forward shapes cannot
# express faithfully. A strategy carrying any of these is skipped with the
# reason surfaced — never backtested against a DIFFERENT close/sizing than live.
_UNSUPPORTED_STOP_OWNERS = (
    "stop_loss_atr_regime", "trailing_stop_atr_regime",  # regime stop owners
    "stop_loss_pct", "trailing_stop_pct", "stop_loss_margin_pct",  # pct stops
)


# ===========================================================================
# Pure — value coercion + neighborhood generation (unit-tested, no data access)
# ===========================================================================

def _is_number(v) -> bool:
    """A tunable numeric value: int/float but NOT bool (a 0/1 toggle is a
    category, not a magnitude — perturbing it is meaningless)."""
    return isinstance(v, (int, float)) and not isinstance(v, bool)


def coerce_scalar(raw: str):
    """Parse a CLI/override scalar into its natural type: int, then float, then
    bool, else the trimmed string. ``--param confirm=1,2`` yields ints so the
    values land in the grid with the same type the registry default carries."""
    s = str(raw).strip()
    try:
        return int(s)
    except ValueError:
        pass
    try:
        return float(s)
    except ValueError:
        pass
    low = s.lower()
    if low in ("true", "false"):
        return low == "true"
    return s


def perturb_numeric(value, step_frac: float, n_steps: int) -> list:
    """Bounded neighborhood offsets around ``value`` (excluding it): ``n_steps``
    on each side at a ``step_frac`` relative step. Ints step by whole units
    (>=1); floats step multiplicatively. A zero value is a disabled/neutral
    sentinel (several params use 0 to mean "off") — never perturbed."""
    if not _is_number(value) or value == 0 or n_steps <= 0 or step_frac <= 0:
        return []
    out = []
    if isinstance(value, int):
        step = max(1, round(abs(value) * step_frac))
        for k in range(1, n_steps + 1):
            out.append(value - k * step)
            out.append(value + k * step)
    else:
        for k in range(1, n_steps + 1):
            out.append(round(value * (1 - k * step_frac), 6))
            out.append(round(value * (1 + k * step_frac), 6))
    return out


def param_neighborhood(live_value, default_range, step_frac: float,
                       n_steps: int) -> list:
    """Candidate values for one tunable param: the live value (always — the
    incumbent must be in the grid), unioned with the strategy's
    ``DEFAULT_PARAM_RANGES`` row (when present) and a bounded perturbation around
    the live value. Numeric lists sort ascending; a categorical param (e.g.
    ``session``) preserves insertion order and is never perturbed."""
    vals = [live_value]
    if default_range:
        vals.extend(default_range)
    vals.extend(perturb_numeric(live_value, step_frac, n_steps))
    # Dedup preserving first-seen order (categorical safety), then sort when the
    # whole set is homogeneously numeric.
    uniq = list(dict.fromkeys(vals))
    if all(_is_number(v) for v in uniq):
        uniq = sorted(uniq)
    return uniq


def effective_params(config_params: dict, default_params: dict) -> dict:
    """The value the live strategy actually runs for every registry param: the
    config value where set, else the (platform-merged) registry default. This is
    the search universe — every param is passed to walk-forward at this value
    (or a swept override), so a config value that differs from the registry
    default is never silently replaced by the default mid-fold."""
    return {p: config_params.get(p, dv) for p, dv in default_params.items()}


def build_search_grid(eff_params: dict, default_row: dict, override_grids: dict,
                      frozen: set, step_frac: float, n_steps: int,
                      value_ok) -> dict:
    """The walk-forward ``param_ranges`` grid: ``{param: [values]}`` over EVERY
    registry param (so each is threaded at its effective value or a sweep).

    Searched dimensions = keys of ``DEFAULT_PARAM_RANGES[name]`` plus operator
    overrides, minus frozen params — the curated tunable set, which bounds the
    combinatorial blow-up. Every other param is pinned to its effective value
    (single-value grid entry). ``value_ok(param, value) -> bool`` drops
    auto-derived neighborhood values that violate a single-parameter registry
    constraint; the live value is ALWAYS kept (it is live-valid by construction).
    Override value lists are used verbatim (already validated loudly upstream)."""
    searched = (set(default_row) | set(override_grids)) - set(frozen)
    grid = {}
    for param, eff_val in eff_params.items():
        if param in override_grids:
            grid[param] = list(override_grids[param])
        elif param in searched:
            vals = param_neighborhood(eff_val, default_row.get(param),
                                      step_frac, n_steps)
            kept = [v for v in vals if v == eff_val or value_ok(param, v)]
            grid[param] = kept or [eff_val]
        else:
            grid[param] = [eff_val]
    return grid


def grid_size(grid: dict) -> int:
    """Cartesian size of a ``{param: [values]}`` grid (the searched count N)."""
    n = 1
    for values in grid.values():
        n *= max(1, len(values))
    return n


# ===========================================================================
# Pure — operator override resolution (fail-loud), disjoint windows, patches
# ===========================================================================

def parse_cli_param_grids(param_args: list) -> dict:
    """Parse repeatable ``--param name=v1,v2,...`` flags into
    ``{name: [coerced values]}``. Raises on a malformed spec or an empty list."""
    out = {}
    for raw in param_args or []:
        if "=" not in raw:
            raise ValueError(
                f"--param must be name=v1,v2,... (got {raw!r})")
        name, _, csv = raw.partition("=")
        name = name.strip()
        values = [coerce_scalar(v) for v in csv.split(",") if v.strip() != ""]
        if not name or not values:
            raise ValueError(
                f"--param {raw!r} needs a name and at least one value")
        out[name] = values
    return out


def resolve_overrides(strategy_id: str, cli_param_grids: dict,
                      overrides_map: dict, known_params: set,
                      check_value) -> tuple:
    """Merge CLI + file overrides for one strategy into ``(override_grids,
    frozen)``, validated FAIL-LOUD.

    - ``cli_param_grids``: ``{param: [values]}`` from ``--param`` (single-strategy
      runs; the caller rejects --param on a fleet run).
    - ``overrides_map``: ``{strategy_id: {"params": {param: [values]},
      "freeze": [param, ...]}}`` from ``--overrides`` (fleet runs).
    An overridden param's value list REPLACES its auto neighborhood; a frozen
    param is pinned to its live value and excluded from search. Unknown param
    names and constraint-violating override values are refused loudly (never
    silently ignored). ``check_value(param, value)`` raises ValueError on a
    value that violates a single-parameter registry constraint."""
    entry = dict((overrides_map or {}).get(strategy_id) or {})
    file_params = dict(entry.get("params") or {})
    frozen = set(entry.get("freeze") or [])

    override_grids = {}
    for param, values in {**file_params, **cli_param_grids}.items():
        if param not in known_params:
            raise ValueError(
                f"{strategy_id}: override for unknown param {param!r} "
                f"(known: {sorted(known_params)})")
        if not isinstance(values, list) or not values:
            raise ValueError(
                f"{strategy_id}: override {param!r} must be a non-empty list "
                f"of values")
        for v in values:
            check_value(param, v)  # raises on constraint violation
        override_grids[param] = list(values)

    bad_frozen = frozen - known_params
    if bad_frozen:
        raise ValueError(
            f"{strategy_id}: freeze names unknown params {sorted(bad_frozen)} "
            f"(known: {sorted(known_params)})")
    both = frozen & set(override_grids)
    if both:
        raise ValueError(
            f"{strategy_id}: params {sorted(both)} are both overridden and "
            f"frozen — a frozen param is pinned to its live value and cannot "
            f"also carry an override grid")
    return override_grids, frozen


def earliest_stage2_start(stage2_windows: list, windows_table: dict) -> str:
    """The earliest start date across the configured stage-2 evidence windows.
    Stage-1 search data must end strictly before this so the two stages never
    share a bar (selection-aware inference safeguard a). Raises on an unknown
    window name (mirrors auto_suggest's window validation)."""
    starts = []
    for w in stage2_windows:
        if w not in windows_table:
            raise ValueError(
                f"unknown stage-2 window {w!r}; known: {list(windows_table)}")
        starts.append(windows_table[w][0])
    if not starts:
        raise ValueError("no stage-2 windows configured")
    return min(starts)


def param_changes(candidate_params: dict, baseline_eff: dict) -> dict:
    """The params where a candidate differs from the live effective baseline —
    the human-readable delta shown next to each proposed patch."""
    return {p: v for p, v in candidate_params.items()
            if baseline_eff.get(p) != v}


def build_patch(strategy_id: str, open_name: str, candidate_params: dict,
                baseline_eff: dict) -> dict:
    """A ready-to-review config patch for one survivor. The patch REPLACES
    ``open_strategy.params`` with the full proposed set (so applying it is a
    single canonical assignment, which #1341's apply flow resolves); the delta
    vs live is recorded separately for the reader. Suggest-only — this is data,
    not an applied change."""
    return {
        "strategy_id": strategy_id,
        "open_strategy": {"name": open_name, "params": dict(candidate_params)},
        "param_changes": param_changes(candidate_params, baseline_eff),
    }


def build_candidate(open_name: str, params: dict, resolution: dict) -> dict:
    """An auto_suggest / eval_windows open-candidate dict carrying the live close
    stack, direction and regime gate around a parameter set. Only the fields the
    backtest candidate shape faithfully expresses are emitted (support is gated
    upstream by ``unsupported_reason``)."""
    cand = {
        "name": open_name,
        "params": dict(params),
        "type": resolution.get("strategy_type") or "perps",
        "direction": resolution.get("direction") or "long",
    }
    if resolution.get("close_strategies"):
        cand["close_strategies"] = copy.deepcopy(resolution["close_strategies"])
    if resolution.get("stop_loss_atr_mult") is not None:
        cand["stop_loss_atr_mult"] = resolution["stop_loss_atr_mult"]
    if resolution.get("trailing_stop_atr_mult") is not None:
        cand["trailing_stop_atr_mult"] = resolution["trailing_stop_atr_mult"]
    if resolution.get("allowed_regimes"):
        cand["allowed_regimes"] = list(resolution["allowed_regimes"])
    if resolution.get("regime_windows_spec"):
        cand["regime_windows_spec"] = copy.deepcopy(resolution["regime_windows_spec"])
    return cand


def unsupported_reason(resolution: dict) -> str | None:
    """Why the tuner cannot faithfully backtest this live resolution, or None.

    The backtest candidate / walk-forward shapes express a scalar ATR stop (or
    no stop) plus standard close refs, long/short/both, and a regime entry gate.
    Regime/pct stop owners, per-regime directional policy, profile allocation,
    signal inversion, and the alternative sizing modes have no faithful bar-level
    replay here — backtesting them against a scalar-stop approximation would
    silently diverge from live, so the strategy is skipped with the reason
    surfaced instead."""
    for key in _UNSUPPORTED_STOP_OWNERS:
        if resolution.get(key) not in (None, 0, 0.0):
            return f"unsupported_stop:{key}"
    if resolution.get("regime_directional_policy"):
        return "unsupported_regime_directional_policy"
    if resolution.get("profile_allocation"):
        return "unsupported_profile_allocation"
    if resolution.get("invert_signal"):
        return "unsupported_invert_signal"
    if resolution.get("risk_per_trade_pct") is not None:
        return "unsupported_risk_per_trade_pct"
    if resolution.get("allow_scale_in"):
        return "unsupported_allow_scale_in"
    return None


def stage1_skip_reason(resolution: dict) -> str | None:
    """Why stage-1 walk-forward search must be skipped (the full neighborhood
    then goes straight to stage 2, which models these faithfully), or None.

    - ``direction='short'``: the walk-forward warmup seeder is long-only and
      would carry a phantom long into the short run (optimizer rejects it too).
    - a composite regime gate (``regime_windows_spec``): walk-forward models only
      the legacy single-lookback ADX regime, so it cannot reproduce the
      composite entry gate — eval_windows (stage 2) can.
    """
    if (resolution.get("direction") or "long") == "short":
        return "short_direction_long_only_seeder"
    if resolution.get("regime_windows_spec"):
        return "composite_regime_gate_unmodelable_in_walk_forward"
    return None


# ===========================================================================
# I/O — config enumeration, stage 1, stage 2, orchestration
# ===========================================================================

def config_strategy_entries(config_path: str, only_id: str | None) -> list:
    """Read the config once and return ``[(strategy_id, symbol, timeframe), ...]``
    for the strategies to tune (all, or the one named). Raises with the same
    context as the loader on a missing id / pre-v15 config is left to
    ``load_strategy_config`` per strategy so its rich rejection text surfaces."""
    with open(config_path) as fh:
        cfg = json.load(fh)
    strategies = cfg.get("strategies") or []
    out = []
    for sc in strategies:
        sid = sc.get("id")
        if only_id is not None and sid != only_id:
            continue
        args = sc.get("args") or []
        symbol = str(args[1]) if len(args) > 1 else None
        regime_tf = str((cfg.get("regime") or {}).get("timeframe") or "").strip()
        timeframe = regime_tf or (str(args[2]) if len(args) > 2 else None)
        out.append((sid, symbol, timeframe))
    if only_id is not None and not out:
        available = [s.get("id") for s in strategies]
        raise ValueError(
            f"{config_path}: no strategy with id={only_id!r}. "
            f"Available: {available}")
    return out


def run_stage1(open_name: str, grid: dict, resolution: dict, symbol: str,
               timeframe: str, registry: str, stage2_start: str,
               n_splits: int, capital: float, metric: str,
               verbose: bool) -> dict:
    """Walk-forward search over ``grid`` on data ending STRICTLY before
    ``stage2_start`` (disjoint evidence). Returns ``{"survivors": [param dicts],
    "n_folds", "n_bars"}`` or ``{"error": reason}``. Raises ValueError with a
    clear message when the disjoint slice leaves no usable data (never silently
    runs stage 1 on an empty/tiny frame)."""
    df = load_cached_data(symbol, timeframe, exchange_id=DATA_PLATFORM,
                          start_date=DEFAULT_SINCE)
    if df is None or df.empty:
        raise ValueError(
            f"no cached {symbol} {timeframe} data from {DEFAULT_SINCE} for "
            f"stage-1 search")
    import pandas as pd
    df = df[df.index < pd.Timestamp(stage2_start)]
    if len(df) < MIN_STAGE1_BARS:
        raise ValueError(
            f"stage-1 disjoint slice leaves only {len(df)} bars before the "
            f"earliest stage-2 window start {stage2_start} (need "
            f">= {MIN_STAGE1_BARS}); stage-1 search would overlap stage-2 "
            f"evidence or have no data. Narrow the stage-2 windows or extend "
            f"cached history.")
    df = _attach_funding_if_needed(df, open_name, symbol, DEFAULT_SINCE)

    summary = walk_forward_optimize(
        df, open_name, grid,
        n_splits=n_splits, initial_capital=capital,
        symbol=symbol, timeframe=timeframe,
        registry=registry, platform=FEE_PLATFORM, verbose=verbose,
        regime_enabled=bool(resolution.get("regime_enabled")),
        regime_period=int(resolution.get("regime_period") or 14),
        regime_adx_threshold=float(resolution.get("regime_adx_threshold") or 20.0),
        allowed_regimes=resolution.get("allowed_regimes"),
        close_strategies=resolution.get("close_strategies") or None,
        stop_loss_atr_mult=resolution.get("stop_loss_atr_mult"),
        trailing_stop_atr_mult=resolution.get("trailing_stop_atr_mult"),
        optimize_metric=metric,
        direction=resolution.get("direction"),
    )
    if summary.get("error"):
        return {"error": summary["error"], "n_bars": int(len(df))}
    # OOS-stable survivors: the distinct best_params that won at least one fold's
    # out-of-sample validation, deduped by canonical JSON.
    seen, survivors = set(), []
    for w in summary.get("window_results") or []:
        bp = w.get("best_params")
        if bp is None:
            continue
        key = json.dumps(bp, sort_keys=True, default=str)
        if key not in seen:
            seen.add(key)
            survivors.append(dict(bp))
    return {
        "survivors": survivors,
        "n_folds": int(summary.get("n_valid_folds") or 0),
        "n_bars": int(len(df)),
    }


def write_stage2_spec(spec_path: str, study: str, registry: str,
                      stage2_windows: list, stage2_datasets: list, alpha: float,
                      family_size: int, candidates: list) -> None:
    """Emit the generated ``suggest.json`` the driver hands to auto_suggest. The
    ``correction.family_size`` is the searched count N (#1338): the BH family is
    corrected against everything stage 1 searched, not just the survivors."""
    spec = {
        "study": study,
        "registry": registry,
        "harnesses": STAGE2_HARNESSES,
        "windows": stage2_windows,
        "datasets": stage2_datasets,
        "correction": {"method": "benjamini_hochberg", "alpha": alpha,
                       "family_size": family_size},
        "candidates": candidates,
    }
    with open(spec_path, "w") as fh:
        json.dump(spec, fh, indent=2, default=str)


def run_stage2(spec_path: str, out_json: str, out_dir: str, jobs: int) -> dict:
    """Drive auto_suggest over the generated spec and return its artifact dict.
    Isolated so tests can monkeypatch it. auto_suggest returns non-zero only when
    a GATE harness run failed — the artifact is still written (with run_failed
    verdicts), so we read it regardless and record the exit code."""
    rc = auto_suggest.main([
        "--spec", spec_path, "--json", out_json,
        "--out-dir", os.path.join(out_dir, "harness_runs"),
        "--jobs", str(jobs),
    ])
    with open(out_json) as fh:
        report = json.load(fh)
    report["_exit_code"] = int(rc)
    return report


def _progress_path(out_dir: str) -> str:
    return os.path.join(out_dir, "tune_live.progress.json")


def write_progress(out_dir: str, payload: dict) -> None:
    """Structured progress a poller reads instead of parsing stdout (#1339). No
    wall-clock timestamp — the artifact and progress stay reproducible/testable;
    phase + strategy index + candidate counts are the poll signal."""
    payload = {"schema_version": SCHEMA_VERSION, "tool": "tune_live", **payload}
    with open(_progress_path(out_dir), "w") as fh:
        json.dump(payload, fh, indent=2, default=str)


def tune_strategy(config_path: str, strategy_id: str, symbol: str,
                  timeframe: str, registry: str, reg_mod, args,
                  overrides_map: dict, out_dir: str) -> dict:
    """Tune one strategy end-to-end. Never raises for a strategy-specific
    problem — records a ``status`` and returns, so a fleet run continues."""
    result = {"strategy_id": strategy_id, "symbol": symbol,
              "timeframe": timeframe, "notes": []}

    # 1. Resolve the live baseline (REUSE the config loader; inherit its v15 gate
    #    and rich rejection text). A load failure is per-strategy, not fatal.
    try:
        resolution = load_strategy_config(config_path, strategy_id,
                                          inject_user_defaults=True)
    except ValueError as exc:
        result["status"] = "config_error"
        result["error"] = str(exc)
        return result

    open_name = resolution["open_strategy"]["name"]
    baseline_params = dict(resolution["open_strategy"]["params"])  # byte-match parity
    result["open_strategy"] = open_name
    result["direction"] = resolution.get("direction")
    result["baseline_params"] = baseline_params
    result["close_strategies"] = copy.deepcopy(resolution.get("close_strategies") or [])
    result["stop_owner"] = {
        k: resolution.get(k) for k in
        ("stop_loss_atr_mult", "trailing_stop_atr_mult",
         "stop_loss_atr_regime", "trailing_stop_atr_regime",
         "stop_loss_pct", "trailing_stop_pct", "stop_loss_margin_pct")
        if resolution.get(k) not in (None, 0, 0.0)
    }

    if not symbol or not timeframe:
        result["status"] = "no_market"
        result["error"] = (f"strategy {strategy_id!r} has no resolvable "
                           f"symbol/timeframe (args[1]/args[2])")
        return result

    reason = unsupported_reason(resolution)
    if reason:
        result["status"] = "unsupported"
        result["reason"] = reason
        return result

    # 2. Effective params + candidate neighborhood.
    if open_name not in reg_mod.STRATEGY_REGISTRY:
        result["status"] = "unknown_open_strategy"
        result["error"] = (f"open strategy {open_name!r} not in {registry} "
                           f"registry")
        return result
    default_params = dict(reg_mod.STRATEGY_REGISTRY[open_name]["default_params"])
    eff = effective_params(baseline_params, default_params)
    known = set(default_params)

    def _check_value(param, value):
        reg_mod.validate_param_value(open_name, param, value)

    def _value_ok(param, value):
        try:
            reg_mod.validate_param_value(open_name, param, value)
            return True
        except ValueError:
            return False

    try:
        cli_grids = parse_cli_param_grids(args.param) if args.param else {}
        override_grids, frozen = resolve_overrides(
            strategy_id, cli_grids, overrides_map, known, _check_value)
    except ValueError as exc:
        result["status"] = "override_error"
        result["error"] = str(exc)
        return result

    default_row = dict(DEFAULT_PARAM_RANGES.get(open_name) or {})
    grid = build_search_grid(eff, default_row, override_grids, frozen,
                             args.step_frac, args.neighborhood_steps, _value_ok)
    n_searched = grid_size(grid)
    result["searched_family_size"] = n_searched
    result["neighborhood"] = {p: v for p, v in grid.items() if len(v) > 1}
    if not default_row and not override_grids:
        result["notes"].append(
            "no DEFAULT_PARAM_RANGES row and no overrides — only the live "
            "baseline is evaluated; pass --param/--overrides to search")

    # 3. Stage 1 — walk-forward search. Skipped when it cannot faithfully model
    #    the live entry universe (short direction / composite regime gate) or in
    #    --dry-run: the FULL neighborhood then becomes the stage-2 candidate set
    #    (generate_param_grid already returns full param dicts).
    stage2_start = earliest_stage2_start(args.windows, M1_WINDOWS)
    skip_reason = "dry_run" if args.dry_run else stage1_skip_reason(resolution)
    if skip_reason:
        result["stage1"] = {"ran": False, "skipped_reason": skip_reason}
        if skip_reason != "dry_run":
            result["notes"].append(f"stage-1 skipped ({skip_reason}); full "
                                   f"neighborhood sent to stage 2")
        survivor_params = generate_param_grid(grid)
    else:
        try:
            s1 = run_stage1(open_name, grid, resolution, symbol, timeframe,
                            registry, stage2_start, args.splits, args.capital,
                            args.optimize_metric, args.verbose)
        except ValueError as exc:
            result["status"] = "stage1_error"
            result["error"] = str(exc)
            return result
        if s1.get("error"):
            result["status"] = "stage1_failed"
            result["error"] = s1["error"]
            result["stage1"] = {"ran": True, **{k: s1[k] for k in s1 if k != "error"}}
            return result
        result["stage1"] = {"ran": True, "n_folds": s1["n_folds"],
                            "n_bars": s1["n_bars"],
                            "n_survivors": len(s1["survivors"])}
        survivor_params = s1["survivors"]

    # 4. Assemble stage-2 candidates: baseline (candidate zero) + survivors,
    #    deduped so a survivor equal to the incumbent is not double-counted.
    baseline_cand = build_candidate(open_name, baseline_params, resolution)
    candidates = [{"key": "baseline", "candidate": baseline_cand,
                   "hypothesis": "live incumbent"}]
    baseline_key = json.dumps({p: eff[p] for p in eff}, sort_keys=True, default=str)
    seen_keys = {baseline_key}
    for i, sp in enumerate(survivor_params):
        full = {**eff, **sp}  # survivor over the effective baseline
        key = json.dumps(full, sort_keys=True, default=str)
        if key in seen_keys:
            continue
        seen_keys.add(key)
        candidates.append({
            "key": f"cand_{i}",
            "candidate": build_candidate(open_name, full, resolution),
        })

    if len(candidates) > args.max_candidates:
        result["status"] = "too_many_candidates"
        result["error"] = (
            f"{len(candidates)} stage-2 candidates exceeds --max-candidates="
            f"{args.max_candidates}; narrow the search with --param/--overrides "
            f"or raise the cap")
        return result
    result["n_candidates"] = len(candidates)

    # 5. Stage 2 — drive auto_suggest with a SELECTION-AWARE family size = N.
    stage2_datasets = args.datasets or [f"{symbol}:{timeframe}"]
    spec_path = os.path.join(out_dir, f"suggest.{strategy_id}.json")
    stage2_json = os.path.join(out_dir, f"stage2.{strategy_id}.json")
    write_stage2_spec(spec_path, f"tune_live_{strategy_id}", registry,
                      args.windows, stage2_datasets, args.alpha, n_searched,
                      candidates)
    result["stage2_spec"] = os.path.relpath(spec_path, _REPO)
    if args.dry_run:
        result["status"] = "dry_run"
        return result
    try:
        report = run_stage2(spec_path, stage2_json, out_dir, args.jobs)
    except Exception as exc:  # noqa: BLE001 — a stage-2 failure is per-strategy
        result["status"] = "stage2_failed"
        result["error"] = f"{type(exc).__name__}: {exc}"
        return result

    # 6. Rank survivors + emit patches.
    result["status"] = "ranked"
    result["correction"] = report.get("correction")
    result["stage2_exit_code"] = report.get("_exit_code")
    ranked = []
    for entry in report.get("ranked") or []:
        cand_params = (entry.get("candidate") or {}).get("params") or {}
        row = {
            "key": entry.get("key"),
            "verdict": entry.get("verdict"),
            "params": cand_params,
            "evidence": entry.get("evidence"),
            "limitations": entry.get("limitations"),
        }
        if entry.get("key") != "baseline" and entry.get("verdict") == "survivor":
            row["patch"] = build_patch(strategy_id, open_name, cand_params, eff)
        ranked.append(row)
    result["ranked"] = ranked
    result["survivors"] = [r for r in ranked if r.get("verdict") == "survivor"
                           and r["key"] != "baseline"]
    return result


def build_parser() -> argparse.ArgumentParser:
    p = argparse.ArgumentParser(description=__doc__.splitlines()[0])
    p.add_argument("--config", required=True, help="Path to a live go-trader config.json")
    p.add_argument("--strategy", default=None,
                   help="Strategy id to tune (default: every strategy in the config)")
    p.add_argument("--registry", choices=["spot", "futures"], default="spot")
    p.add_argument("--datasets", default=None,
                   help="Stage-2 datasets, comma list SYMBOL:TIMEFRAME "
                        "(default: the strategy's own market)")
    p.add_argument("--windows", default=None,
                   help=f"Stage-2 evidence windows, comma list "
                        f"(default: {','.join(DEFAULT_STAGE2_WINDOWS)})")
    p.add_argument("--param", action="append", default=None, metavar="NAME=V1,V2",
                   help="Override one param's search values (single-strategy runs "
                        "only). Repeatable. Replaces the auto neighborhood.")
    p.add_argument("--overrides", default=None,
                   help="Path to a JSON overrides file: {strategy_id: {params: "
                        "{name: [values]}, freeze: [names]}} (fleet runs).")
    p.add_argument("--neighborhood-steps", type=int, default=0, dest="neighborhood_steps",
                   help="Perturbation steps each side of the live value added to "
                        "the DEFAULT_PARAM_RANGES neighborhood (0 = range + live "
                        "value only).")
    p.add_argument("--step-frac", type=float, default=0.25, dest="step_frac",
                   help="Relative perturbation step size (default 0.25).")
    p.add_argument("--splits", type=int, default=DEFAULT_SPLITS,
                   help="Walk-forward splits for stage 1")
    p.add_argument("--capital", type=float, default=DEFAULT_CAPITAL)
    p.add_argument("--optimize-metric", default=DEFAULT_METRIC, dest="optimize_metric",
                   help="Stage-1 walk-forward selection metric")
    p.add_argument("--alpha", type=float, default=DEFAULT_ALPHA,
                   help="Stage-2 BH correction alpha")
    p.add_argument("--max-candidates", type=int, default=DEFAULT_MAX_CANDIDATES,
                   dest="max_candidates",
                   help="Refuse a strategy whose stage-2 candidate count exceeds "
                        "this (guards the stage-1-skipped full-neighborhood path)")
    p.add_argument("--jobs", type=int, default=4, help="Stage-2 harness parallelism")
    p.add_argument("--out-dir", default=None, dest="out_dir",
                   help="Artifact directory (default: <config dir>/tune_live_runs)")
    p.add_argument("--json", default=None, dest="json_out",
                   help="Write the tuning artifact to this path")
    p.add_argument("--dry-run", action="store_true", dest="dry_run",
                   help="Resolve baselines + emit specs, run no harnesses")
    p.add_argument("--verbose", action="store_true", help="Verbose walk-forward output")
    return p


def main(argv=None) -> int:
    args = build_parser().parse_args(argv)
    args.windows = ([w.strip() for w in args.windows.split(",") if w.strip()]
                    if args.windows else list(DEFAULT_STAGE2_WINDOWS))
    args.datasets = ([d.strip() for d in args.datasets.split(",") if d.strip()]
                     if args.datasets else None)

    config_path = os.path.abspath(args.config)
    entries = config_strategy_entries(config_path, args.strategy)
    if args.param and len(entries) > 1:
        raise SystemExit("--param applies to a single strategy; use --strategy "
                         "<id>, or --overrides <file> for a fleet run")
    # Validate window names up front (loud, before any work).
    earliest_stage2_start(args.windows, M1_WINDOWS)

    overrides_map = {}
    if args.overrides:
        with open(args.overrides) as fh:
            overrides_map = json.load(fh)
        if not isinstance(overrides_map, dict):
            raise SystemExit("--overrides file must be a JSON object "
                             "{strategy_id: {...}}")
        # Reject an override keyed to a strategy id the config does not define —
        # a silently-ignored override reads as "applied" when it never ran.
        all_ids = {sid for sid, _, _ in config_strategy_entries(config_path, None)}
        unknown_ids = set(overrides_map) - all_ids
        if unknown_ids:
            raise SystemExit(
                f"--overrides references unknown strategy ids "
                f"{sorted(unknown_ids)}; the config defines {sorted(all_ids)}")

    out_dir = args.out_dir or os.path.join(os.path.dirname(config_path),
                                           "tune_live_runs")
    os.makedirs(out_dir, exist_ok=True)
    reg_mod = load_registry(args.registry)

    n = len(entries)
    write_progress(out_dir, {"phase": "start", "n_strategies": n,
                             "strategy_index": 0})
    results = []
    for i, (sid, symbol, timeframe) in enumerate(entries):
        write_progress(out_dir, {
            "phase": "tuning", "strategy": sid, "strategy_index": i,
            "n_strategies": n})
        res = tune_strategy(config_path, sid, symbol, timeframe, args.registry,
                            reg_mod, args, overrides_map, out_dir)
        results.append(res)
        write_progress(out_dir, {
            "phase": "tuned", "strategy": sid, "strategy_index": i + 1,
            "n_strategies": n, "status": res.get("status"),
            "candidates": res.get("n_candidates"),
            "survivors": len(res.get("survivors") or [])})

    artifact = {
        "schema_version": SCHEMA_VERSION,
        "tool": "tune_live",
        "issue": ISSUE,
        "config": config_path,
        "registry": args.registry,
        "stage2_windows": args.windows,
        "correction_alpha": args.alpha,
        "footer": FOOTER,
        "strategies": results,
    }
    write_progress(out_dir, {"phase": "done", "n_strategies": n,
                             "strategy_index": n})

    print(format_summary(artifact))
    json_out = args.json_out or os.path.join(out_dir, "tune_live.artifact.json")
    with open(json_out, "w") as fh:
        json.dump(artifact, fh, indent=2, default=str)
    print(f"\nwrote {json_out}")
    print(FOOTER)
    # Non-zero only on a hard usage/config failure; per-strategy statuses are
    # data in the artifact, not a process failure (a fleet run surfaces every
    # strategy's outcome regardless).
    return 0


def format_summary(artifact: dict) -> str:
    lines = [f"== tune_live: {os.path.basename(artifact['config'])} "
             f"(schema v{artifact['schema_version']}) =="]
    for r in artifact["strategies"]:
        head = f"  {r['strategy_id']:<28} {r.get('status', '?'):<20}"
        if r.get("status") == "ranked":
            nsurv = len(r.get("survivors") or [])
            head += (f" N={r.get('searched_family_size')} "
                     f"cands={r.get('n_candidates')} survivors={nsurv}")
            corr = r.get("correction") or {}
            if corr:
                head += f" [BH m={corr.get('m')} thr={corr.get('effective_threshold')}]"
        elif r.get("error"):
            head += f" {str(r['error'])[:80]}"
        elif r.get("reason"):
            head += f" {r['reason']}"
        lines.append(head)
        for s in r.get("survivors") or []:
            lines.append(f"      SURVIVOR {s['key']}: "
                         f"{(s.get('patch') or {}).get('param_changes')}")
    lines.append("")
    lines.append(FOOTER)
    return "\n".join(lines)


if __name__ == "__main__":
    raise SystemExit(main())

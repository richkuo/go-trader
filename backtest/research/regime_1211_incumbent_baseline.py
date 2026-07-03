"""Reproducible evidence for #1211: is the regime-promotion gate's incumbent baseline
trustworthy, or must the gate's shipping rule be redesigned?

Background. `regime_calibrate.gate_verdict` hard-gates `ship` on `incumbent_trustworthy`
(the hand-rule's OWN forward-volatility separation must be block-shuffle significant, p <=
SIGNIFICANCE_ALPHA). #1080 measured that separation at OOS p=10/201~=0.0498 with n_perm=200
-- a knife-edge pass. #1095 re-measured it at n_perm=1799 and it was NOT significant
(p=0.105/0.113); #1177 recorded the downgrade. Consequence: every candidate verdict abstains
regardless of candidate quality, and #1074 (live wiring) is dormant with no path to unblock.

This harness produces the evidence to choose ONE of two explicit outcomes (never a silent
relaxation of the threshold):

  (2a) EVIDENCE REPLICATES -- a wider, family-corrected re-measurement shows the incumbent's
       forward-volatility separation is real and the original single OOS window was merely
       underpowered. `incumbent_trustworthy` is restored honestly, no gate code change.
  (2b) EVIDENCE DOES NOT REPLICATE -- the incumbent veto is dropped from `gate_verdict` as a
       deliberate design decision: a candidate ships on its OWN significant separation
       (`model_separation_real`, already required) plus non-inferiority (the KW-H tolerance,
       already required) plus the stability gain, rather than on the incumbent's significance.

Two measurements, two different questions:

  1a POWER ANALYSIS (single-window, alpha=SIGNIFICANCE_ALPHA, n_perm matched to #1177): was the
     original OOS BTC/USDT 1h measurement underpowered for the effect the in-sample window
     shows? Minimum-detectable-effect via simulation. If the window cannot detect the IS-sized
     effect at 80% power, the #1177 downgrade is "inconclusive at that resolution", not a
     negative -- but this NEVER by itself restores incumbent_trustworthy.

  1b RE-MEASUREMENT (ONE Bonferroni family over held-out windows x assets, #1160 permutation
     discipline): does the incumbent separate forward volatility across defensible held-out
     cells at the family-corrected alpha? The family denominator is fixed from data
     availability BEFORE any p-value is computed; unavailable cells are excluded loudly.

  1c DECISION (pre-registered pure rule, committed before the data run): `replication_verdict`
     returns replicates = primary_met AND breadth_met AND symbols_met. The Phase-2 branch is
     exactly this boolean -- no judgement call after seeing p-values.

Run (needs the OHLCV cache reachable from shared_tools/):

    uv run --no-sync python backtest/research/regime_1211_incumbent_baseline.py \
        --json backtest/research/regime_1211_baseline_remeasure.json

Read-only research layer. No live path, no Go path, no config, no gate threshold is touched by
running this. The gate stays fail-closed (abstaining) until a follow-up PR lands one outcome.
"""
from __future__ import annotations
import os, sys

_THIS_DIR = os.path.dirname(os.path.abspath(__file__))
_BACKTEST = os.path.abspath(os.path.join(_THIS_DIR, ".."))
_ROOT = os.path.abspath(os.path.join(_BACKTEST, ".."))
for _p in (_BACKTEST, _ROOT, os.path.join(_ROOT, "shared_tools")):
    if _p not in sys.path:
        sys.path.insert(0, _p)

import importlib.util

import numpy as np

from regime import (compute_regime_composite, composite_feature_matrix,
                    _DEFAULT_COMPOSITE_THRESHOLDS)
from data_fetcher import load_cached_data
from eval_windows import DATASETS, WINDOWS, PLATFORM
from regime_diagnostics import (DEFAULT_PERIOD, forward_realized_vol, separation,
                                stability, block_shuffle_pvalue)
from regime_calibrate import SIGNIFICANCE_ALPHA

# Held-out confirmatory family. `is` is the incumbent-design window (excluded from confirmatory
# evidence; reported descriptively only). The full grid is DATASETS x these windows.
FAMILY_WINDOWS = ("oos", "2023", "2024", "2025H1")
PRIMARY_SYMBOL, PRIMARY_TIMEFRAME = "BTC/USDT", "1h"
PRIMARY_HORIZON = 4                      # the gate's pre-registered primary horizon (h4)
MIN_VALID_BARS = 200                     # below this a cell is too warmed-out to score -> unavailable
# n_perm for the confirmatory re-measurement, matched to the #1095/#1177 resolution so the
# incumbent is judged at exactly the resolution its downgrade was recorded at.
DEFAULT_REMEASURE_N_PERM = 1799
# Power-analysis defaults (single-window, alpha=SIGNIFICANCE_ALPHA). power_n_perm need only
# resolve alpha=0.05 (floor ~19); 499 gives comfortable resolution far faster than 1799 and the
# power estimate is essentially unchanged at this alpha. n_sim=150 gives a power SE ~0.03 near
# the 0.8 threshold -- ample for the supplementary "was the window underpowered" reframing (the
# branch decision is the re-measurement, not this). The lam grid brackets lam=1 (the IS-sized
# effect) on both sides so the MDE crossing is interpolable.
DEFAULT_POWER_N_SIM = 150
DEFAULT_POWER_N_PERM = 499
DEFAULT_LAM_GRID = (0.0, 0.5, 1.0, 1.5, 2.0)
POWER_TARGET = 0.80


def _load_1080():
    """Single source of truth for the #1160 permutation-resolution discipline: `bonferroni_alpha`,
    `resolve_bakeoff_n_perm`, `permutation_steps_to_alpha`, `verdict_knife_edge`. Loaded by path
    (the same pattern #1095 uses) so the open-vs-close `registry.py` import order is untouched."""
    path = os.path.join(_THIS_DIR, "regime_1080_unsupervised_vol_model.py")
    spec = importlib.util.spec_from_file_location("regime_1080_for_1211", path)
    mod = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(mod)
    return mod


# --------------------------------------------------------------------------------------------
# Pure helpers (unit-tested without data access).
# --------------------------------------------------------------------------------------------

def epsilon_squared(kruskal_h: float, n: int, k: int) -> float:
    """Kruskal-Wallis effect size e^2 = (H - k + 1) / (n - k), clamped to [0, 1]. Comparable
    across windows of different length, unlike raw H (which scales with n). n = number of bars
    with a valid forward value; k = number of populated states."""
    if n <= k or k < 1:
        return float("nan")
    val = (float(kruskal_h) - k + 1.0) / (n - k)
    return float(min(1.0, max(0.0, val)))


def forward_vol_profile(labels, fwd) -> dict:
    """Per-state multiplicative forward-volatility profile: median(fwd | state) / median(fwd).
    This is the shape of the separation the in-sample window shows; the power analysis injects
    scaled versions of it. NaN forward values are dropped. Pooled median of 0 -> flat profile."""
    labels = np.asarray(labels, dtype=object)
    fwd = np.asarray(fwd, dtype=float)
    valid = ~np.isnan(fwd)
    labels, fwd = labels[valid], fwd[valid]
    if len(fwd) == 0:
        return {}
    pooled = float(np.median(fwd))
    if pooled <= 0:
        return {str(s): 1.0 for s in sorted(set(labels.tolist()))}
    prof = {}
    for s in sorted(set(labels.tolist())):
        vals = fwd[labels == s]
        prof[str(s)] = float(np.median(vals) / pooled) if len(vals) else 1.0
    return prof


def inject_effect(labels, fwd, profile: dict, lam: float):
    """Scale each bar's forward value by profile[state] ** lam. lam=0 -> identity (no injected
    between-state effect); lam=1 -> the IS-sized effect; lam>1 -> amplified. Rank-preserving and
    strictly multiplicative, so it never manufactures negative volatilities. States absent from
    `profile` default to ratio 1.0."""
    labels = np.asarray(labels, dtype=object)
    fwd = np.asarray(fwd, dtype=float)
    scale = np.array([float(profile.get(str(s), 1.0)) ** float(lam) for s in labels], dtype=float)
    return fwd * scale


def block_bootstrap(source, block_len: int, n_out: int, rng) -> np.ndarray:
    """Circular block bootstrap of `source`: draw random start indices, take wrap-around blocks
    of `block_len`, concatenate to length >= n_out, truncate. Preserves local autocorrelation
    while randomising position -- used to synthesise a label-INDEPENDENT forward series (the
    null) that still carries realistic serial structure."""
    source = np.asarray(source, dtype=float)
    n = len(source)
    block_len = max(1, int(block_len))
    if n == 0:
        return np.zeros(n_out, dtype=float)
    out = []
    total = 0
    while total < n_out:
        start = int(rng.integers(0, n))
        idx = (start + np.arange(block_len)) % n
        out.append(source[idx])
        total += block_len
    return np.concatenate(out)[:n_out]


def simulate_power(labels, fwd, block_len, profile, lam_grid, *, n_sim, n_perm, alpha, seed):
    """Minimum-detectable-effect simulation for the single-window separation test.

    Construction (the null must break the label<->fwd association, so power at lam=0 == alpha):
      - the observed label sequence is held FIXED (preserving its autocorrelation and therefore
        the exact block_len the permutation test derives from label mean-dwell);
      - a null forward series is block-bootstrapped from the POOLED observed forward values,
        independently of labels (no between-state effect);
      - inject_effect scales it by profile ** lam to plant a controlled between-state effect;
      - the block-shuffle permutation test is run at n_perm; power(lam) = P(p <= alpha).

    NOTE: this deliberately does NOT joint-resample (label, fwd) pairs -- joint resampling
    preserves the observed association, so lam=0 would reproduce the observed effect rather than
    a null, and the curve would not measure detectable effect size.
    """
    labels = np.asarray(labels, dtype=object)
    fwd = np.asarray(fwd, dtype=float)
    valid = ~np.isnan(fwd)
    labels_v, fwd_v = labels[valid], fwd[valid]
    n = len(fwd_v)
    rng = np.random.default_rng(seed)
    powers = {}
    for lam in lam_grid:
        hits = 0
        for _ in range(n_sim):
            null_fwd = block_bootstrap(fwd_v, block_len, n, rng)
            planted = inject_effect(labels_v, null_fwd, profile, lam)
            perm_seed = int(rng.integers(0, 2**31 - 1))
            p = block_shuffle_pvalue(labels_v, planted, block_len,
                                     n_perm=n_perm, seed=perm_seed)["p_value"]
            if p <= alpha:
                hits += 1
        powers[float(lam)] = hits / float(n_sim)
    return powers


def min_detectable_effect(lam_grid, powers: dict, target: float = POWER_TARGET):
    """Smallest lam reaching `target` power, linearly interpolated between the bracketing grid
    points. None if the grid never reaches target power (window underpowered across the grid)."""
    grid = sorted(float(l) for l in lam_grid)
    prev_l = prev_p = None
    for l in grid:
        p = powers[l]
        if p >= target:
            if prev_l is None or prev_p is None or p == prev_p:
                return float(l)
            frac = (target - prev_p) / (p - prev_p)
            return float(prev_l + frac * (l - prev_l))
        prev_l, prev_p = l, p
    return None


def replication_verdict(cells: list, alpha_family: float, *,
                        primary_symbol=PRIMARY_SYMBOL, primary_timeframe=PRIMARY_TIMEFRAME) -> dict:
    """Pre-registered decision rule (committed before the data run). `cells` is the list of
    SCORED family cells (available only), each a dict with symbol/timeframe/window/p_value/
    knife_edge/seed_stable. replicates = primary_met AND breadth_met AND symbols_met:

      - primary_met: some primary (BTC/USDT 1h) held-out cell is significant at alpha_family,
        NOT knife-edge, AND seed-stable;
      - breadth_met: at least half of all available cells are significant at alpha_family;
      - symbols_met: the significant cells span >= 2 distinct symbols.

    Ties/edges resolve conservatively (fail-closed): missing seed_stable/knife_edge treated as
    the blocking value. A False here selects Phase-2 branch 2b (gate-semantics revision)."""
    def sig(c):
        return float(c["p_value"]) <= float(alpha_family)

    n_avail = len(cells)
    sig_cells = [c for c in cells if sig(c)]
    primary_met = any(
        c["symbol"] == primary_symbol and c["timeframe"] == primary_timeframe
        and sig(c) and (c.get("knife_edge") is False) and (c.get("seed_stable") is True)
        for c in cells
    )
    breadth_met = n_avail > 0 and (len(sig_cells) * 2 >= n_avail)
    symbols_met = len({c["symbol"] for c in sig_cells}) >= 2
    return {
        "replicates": bool(primary_met and breadth_met and symbols_met),
        "primary_met": bool(primary_met),
        "breadth_met": bool(breadth_met),
        "symbols_met": bool(symbols_met),
        "n_available_cells": int(n_avail),
        "n_significant_cells": int(len(sig_cells)),
        "significant_symbols": sorted({c["symbol"] for c in sig_cells}),
        "alpha_family": float(alpha_family),
    }


# --------------------------------------------------------------------------------------------
# Data layer (mirrors regime_diagnostics.run_window internals so the power analysis and the
# re-measurement score the identical valid-bar mask / block_len as the incumbent gate).
# --------------------------------------------------------------------------------------------

def load_cell(symbol, timeframe, window, horizon=PRIMARY_HORIZON, period=DEFAULT_PERIOD):
    """Reproduce run_window -> score_labels(target='volatility') up to the point of the p-value:
    hand-rule composite labels, the identical NaN feature mask, the forward-vol target, and the
    label-mean-dwell block_len. Returns a cell dict, or {'status': 'unavailable', ...} when the
    cache is missing/short. Computes NO p-value -- availability is decided before any scoring."""
    cell = {"symbol": symbol, "timeframe": timeframe, "window": window, "horizon": int(horizon)}
    try:
        start, end = WINDOWS[window]
        df = load_cached_data(symbol, timeframe, exchange_id=PLATFORM,
                              start_date=start, end_date=end)
    except Exception as exc:  # noqa: BLE001 -- any cache/fetch failure = unavailable, logged
        cell.update({"status": "unavailable", "reason": f"load_failed: {exc}"})
        return cell
    if df is None or len(df) < MIN_VALID_BARS:
        cell.update({"status": "unavailable",
                     "reason": f"too_few_bars: {0 if df is None else len(df)} < {MIN_VALID_BARS}"})
        return cell
    # Fail-closed cache-stability guard: this repo's live scheduler backfills the shared OHLCV
    # cache continuously, and a read caught mid-write can return duplicated or out-of-order rows
    # that silently inflate n and distort the Kruskal-H / p-value (observed: a fixed window
    # scoring H=672 with n implying ~30k bars for a single year). Reject such a cell loudly rather
    # than score a corrupt dataset -- reproducibility of the verdict depends on a quiescent cache.
    dup_ts = int(df.index.duplicated().sum())
    if dup_ts or not df.index.is_monotonic_increasing:
        cell.update({"status": "unavailable",
                     "reason": f"cache_unstable: dup_timestamps={dup_ts} "
                               f"monotonic={bool(df.index.is_monotonic_increasing)} "
                               f"(re-run on a quiescent cache)"})
        return cell
    th = dict(_DEFAULT_COMPOSITE_THRESHOLDS)
    features = composite_feature_matrix(df, period, th).to_numpy()
    labels = compute_regime_composite(df, period=period, thresholds=th)["regime"].to_numpy()
    valid = ~np.isnan(features).any(axis=1)
    vlabels = labels[valid]
    if len(vlabels) < MIN_VALID_BARS:
        cell.update({"status": "unavailable",
                     "reason": f"too_few_valid_bars: {int(valid.sum())} < {MIN_VALID_BARS}"})
        return cell
    st = stability(vlabels)
    mean_dwell = float(np.mean(list(st["mean_dwell"].values()))) if st["mean_dwell"] else 1.0
    block_len = max(int(3 * mean_dwell), int(horizon))
    fwd = forward_realized_vol(df["close"].to_numpy(), horizon)[valid]
    sep = separation(vlabels, fwd)
    n_eff = int(sum(s["n"] for s in sep["per_state"].values()))
    k = int(len(sep["per_state"]))
    # Pin the resolved span for reproducibility (`oos` end=None resolves to the latest cached bar).
    idx = df.index
    cell.update({
        "status": "available",
        "labels": vlabels,
        "fwd": fwd,
        "block_len": int(block_len),
        "n_valid": int(len(vlabels)),
        "n_eff": n_eff,
        "k_states": k,
        "kruskal_h": float(sep["kruskal_h"]),
        "epsilon_squared": epsilon_squared(sep["kruskal_h"], n_eff, k),
        "transition_rate": float(st["transition_rate"]),
        "resolved_start": str(idx[0]) if len(idx) else None,
        "resolved_end": str(idx[-1]) if len(idx) else None,
        "n_bars_raw": int(len(df)),
        "dup_timestamps": dup_ts,
    })
    return cell


def _public(cell: dict) -> dict:
    """Cell dict without the heavy numpy arrays -- what goes into the JSON artifact."""
    return {k: v for k, v in cell.items() if k not in ("labels", "fwd")}


# --------------------------------------------------------------------------------------------
# Orchestration.
# --------------------------------------------------------------------------------------------

def run_power(power_cells, *, n_sim, n_perm, alpha, lam_grid, seed, log=None):
    """1a: minimum-detectable-effect on each requested single window, against the IS-window
    effect profile (BTC/USDT 1h `is`). alpha = SIGNIFICANCE_ALPHA (the single-window bar the
    #1177 downgrade was recorded at), NOT the family alpha."""
    log = log or (lambda *_: None)
    is_cell = load_cell(PRIMARY_SYMBOL, PRIMARY_TIMEFRAME, "is")
    if is_cell["status"] != "available":
        return {"status": "unavailable", "reason": "is_window_unavailable: " + is_cell.get("reason", "")}
    profile = forward_vol_profile(is_cell["labels"], is_cell["fwd"])
    results = []
    for (symbol, timeframe, window) in power_cells:
        cell = load_cell(symbol, timeframe, window)
        if cell["status"] != "available":
            log(f"[power] {symbol} {timeframe} {window}: {cell['reason']}")
            results.append({"symbol": symbol, "timeframe": timeframe, "window": window,
                            "status": cell["status"], "reason": cell.get("reason")})
            continue
        log(f"[power] {symbol} {timeframe} {window}: n_valid={cell['n_valid']} "
            f"block_len={cell['block_len']} sims={n_sim} n_perm={n_perm}")
        powers = simulate_power(cell["labels"], cell["fwd"], cell["block_len"], profile,
                                lam_grid, n_sim=n_sim, n_perm=n_perm, alpha=alpha, seed=seed)
        mde = min_detectable_effect(lam_grid, powers)
        entry = _public(cell)
        entry.update({
            "power_curve": {str(k): v for k, v in powers.items()},
            "power_at_is_effect": powers.get(1.0),
            "mde_lambda": mde,
            "powered_for_is_effect": bool(powers.get(1.0, 0.0) >= POWER_TARGET),
            "alpha": float(alpha), "n_sim": int(n_sim), "power_n_perm": int(n_perm),
        })
        results.append(entry)
    return {"status": "ok", "effect_profile": profile,
            "profile_window": f"{PRIMARY_SYMBOL} {PRIMARY_TIMEFRAME} is", "cells": results}


def run_remeasure(datasets, windows, *, requested_n_perm, seed, seed_stability_seeds=(0, 1, 2),
                  log=None):
    """1b: ONE Bonferroni family over (datasets x windows). Availability -> family denominator ->
    n_perm resolution ALL happen before any p-value is computed."""
    log = log or (lambda *_: None)
    m1080 = _load_1080()
    # Load every cell first (no p-values yet) so the family denominator is fixed from availability.
    loaded = {}
    for (symbol, timeframe) in datasets:
        for window in windows:
            cell = load_cell(symbol, timeframe, window)
            loaded[(symbol, timeframe, window)] = cell
            if cell["status"] != "available":
                log(f"[remeasure] UNAVAILABLE {symbol} {timeframe} {window}: {cell['reason']}")
    available = [key for key, c in loaded.items() if c["status"] == "available"]
    n_avail = len(available)
    if n_avail == 0:
        return {"status": "no_cells", "cells": [], "unavailable": [
            {"symbol": s, "timeframe": t, "window": w, "reason": loaded[(s, t, w)].get("reason")}
            for (s, t, w) in loaded if loaded[(s, t, w)]["status"] != "available"]}
    alpha_family = m1080.bonferroni_alpha(n_avail)
    n_perm = m1080.resolve_bakeoff_n_perm(n_avail, requested=requested_n_perm)
    log(f"[remeasure] {n_avail} available cells -> alpha_family={alpha_family:.6f} n_perm={n_perm}")

    scored = []
    for key in available:
        symbol, timeframe, window = key
        cell = loaded[key]
        pv = block_shuffle_pvalue(cell["labels"], cell["fwd"], cell["block_len"],
                                  n_perm=n_perm, seed=seed)
        steps = m1080.permutation_steps_to_alpha(pv["p_value"], n_perm, alpha=alpha_family)
        is_primary = (symbol == PRIMARY_SYMBOL and timeframe == PRIMARY_TIMEFRAME)
        seed_stable = None
        seed_p = None
        if is_primary:
            # Seed-stability on the primary cells: a verdict that flips significance across seeds
            # is treated as unstable and blocks branch 2a (via replication_verdict).
            seed_p = [block_shuffle_pvalue(cell["labels"], cell["fwd"], cell["block_len"],
                                           n_perm=n_perm, seed=s)["p_value"]
                      for s in seed_stability_seeds]
            sig_flags = [p <= alpha_family for p in seed_p]
            seed_stable = bool(all(sig_flags) or not any(sig_flags))
        entry = _public(cell)
        entry.update({
            "p_value": float(pv["p_value"]),
            "n_perm": int(n_perm),
            "alpha_family": float(alpha_family),
            "significant": bool(pv["p_value"] <= alpha_family),
            "permutation_steps_to_alpha": int(steps),
            "knife_edge": bool(m1080.verdict_knife_edge(steps)),
            "is_primary": bool(is_primary),
            "seed_stability_p": seed_p,
            "seed_stable": seed_stable,
        })
        scored.append(entry)
        log(f"[remeasure] {symbol} {timeframe} {window}: p={pv['p_value']:.5f} "
            f"h={cell['kruskal_h']:.2f} eps2={cell['epsilon_squared']:.4f} "
            f"sig={entry['significant']} knife_edge={entry['knife_edge']}")

    verdict = replication_verdict(scored, alpha_family)
    return {
        "status": "ok",
        "n_available_cells": n_avail,
        "alpha_family": float(alpha_family),
        "n_perm": int(n_perm),
        "cells": scored,
        "unavailable": [
            {"symbol": s, "timeframe": t, "window": w, "reason": loaded[(s, t, w)].get("reason")}
            for (s, t, w) in loaded if loaded[(s, t, w)]["status"] != "available"],
        "replication_verdict": verdict,
    }


def build_parser():
    import argparse
    p = argparse.ArgumentParser(description="#1211 incumbent-baseline power + re-measurement")
    p.add_argument("--datasets", default="", help="comma SYMBOL:TIMEFRAME; default eval_windows.DATASETS")
    p.add_argument("--windows", default=",".join(FAMILY_WINDOWS),
                   help=f"held-out family windows; known: {', '.join(WINDOWS)}")
    p.add_argument("--remeasure-n-perm", type=int, default=DEFAULT_REMEASURE_N_PERM,
                   help="requested permutation count for the confirmatory family (floored by #1160)")
    p.add_argument("--power-cells", default="BTC/USDT:1h:oos",
                   help="comma SYMBOL:TIMEFRAME:WINDOW for the power analysis (default: the exact "
                        "#1177 OOS window; add BTC/USDT:1h:2023 to contrast a full-year window)")
    p.add_argument("--n-sim", type=int, default=DEFAULT_POWER_N_SIM)
    p.add_argument("--power-n-perm", type=int, default=DEFAULT_POWER_N_PERM)
    p.add_argument("--lam-grid", default=",".join(str(x) for x in DEFAULT_LAM_GRID))
    p.add_argument("--seed", type=int, default=0)
    p.add_argument("--skip-power", action="store_true")
    p.add_argument("--skip-remeasure", action="store_true")
    p.add_argument("--json", default=None, help="write the report JSON here")
    return p


def _parse_datasets(spec):
    if not spec.strip():
        return list(DATASETS)
    out = []
    for tok in spec.split(","):
        sym, tf = tok.split(":")
        out.append((sym.strip(), tf.strip()))
    return out


def _parse_power_cells(spec):
    out = []
    for tok in spec.split(","):
        if not tok.strip():
            continue
        sym, tf, win = tok.split(":")
        out.append((sym.strip(), tf.strip(), win.strip()))
    return out


def main(argv=None) -> int:
    import json
    args = build_parser().parse_args(argv)
    log = lambda msg: print(msg, file=sys.stderr)
    datasets = _parse_datasets(args.datasets)
    windows = tuple(w.strip() for w in args.windows.split(",") if w.strip())
    for w in windows:
        if w not in WINDOWS:
            raise SystemExit(f"unknown window {w}; known: {list(WINDOWS)}")
    lam_grid = tuple(float(x) for x in args.lam_grid.split(","))

    payload = {
        "issue": 1211,
        "significance_alpha": float(SIGNIFICANCE_ALPHA),
        "primary": {"symbol": PRIMARY_SYMBOL, "timeframe": PRIMARY_TIMEFRAME,
                    "horizon": PRIMARY_HORIZON},
        "seed": int(args.seed),
    }

    # Re-measurement first: it carries the pre-registered decision that selects the Phase-2 branch.
    # The power analysis is supplementary reframing, so it runs after.
    if not args.skip_remeasure:
        log("== 1b re-measurement (one Bonferroni family) ==")
        payload["remeasurement"] = run_remeasure(
            datasets, windows, requested_n_perm=args.remeasure_n_perm, seed=args.seed, log=log)
        rv = payload["remeasurement"].get("replication_verdict")
        if rv is not None:
            branch = "2a (evidence replicates)" if rv["replicates"] else "2b (revise gate semantics)"
            log(f"== decision: replicates={rv['replicates']} -> Phase-2 branch {branch} ==")

    if not args.skip_power:
        log("== 1a power analysis ==")
        payload["power_analysis"] = run_power(
            _parse_power_cells(args.power_cells), n_sim=args.n_sim, n_perm=args.power_n_perm,
            alpha=SIGNIFICANCE_ALPHA, lam_grid=lam_grid, seed=args.seed, log=log)

    text = json.dumps(payload, indent=2, default=float)
    if args.json:
        with open(args.json, "w") as fh:
            fh.write(text)
    print(text)
    return 0


if __name__ == "__main__":
    raise SystemExit(main())

"""Producer for the directional-certification artifact (#1085).

SSoT for the regime->direction edge gate. Re-runs the #1076 scope-1 per-state
directional screen (regime_1076_directional_premise.run) and emits
``regime_directional_certifications.json`` — the artifact consumed by BOTH the
live Go daemon (scheduler/regime_directional_certification.go) and the
backtester (backtest/directional_certification.py). Keeping the statistical test
HERE, in one place, is the whole point: Go never reimplements the test.

Certification criterion (multiplicity-honest, mirrors the premise script's
report()):
  A (asset, timeframe, classifier) cell is CERTIFIED for a canonical trend
  direction only when a directional state for that direction:
    1. survives GLOBAL Benjamini-Hochberg FDR across the WHOLE directional
       family (not merely the within-cell BH the screen also computes), AND
    2. is sign-aligned with the policy bet (trending_up -> long, trending_down
       -> short), AND
    3. persists in a HELD-OUT-FORWARD window (is/oos) — the windows the live
       policy must actually work in; a historical-only hit is overfit.

Under the current universe NOTHING survives global correction (#1076: 0/2121),
so the emitted artifact certifies nothing and every regime_directional_policy
runs default-off in live and backtest. Re-run this when new data or a new
classifier might change that; the artifact carries an expiry (default 90 days)
so a stale certification fails closed.

Run (needs the OHLCV cache reachable from shared_tools/):

    uv run --no-sync python backtest/research/regime_1076_certify.py
    uv run --no-sync python backtest/research/regime_1076_certify.py \
        --symbols "BTC/USDT,ETH/USDT,SOL/USDT,HYPE/USDC:USDC@hyperliquid"

#1443 — hard rules this producer now ENFORCES rather than merely documents. Every one
of them guards the same thing: a REPO-TRACKED output (the artifact the live daemon reads,
and the committed run report that is this issue's evidence) may only be written by a run
that was actually capable of certifying, over the baseline universe, on the baseline data.

1. FAMILY INTEGRITY. certify() applies global BH over only the rows of the CURRENT
   invocation, so a narrowed run (fewer symbols/timeframes/windows/classifiers/horizons)
   shrinks the correction family and inflates every cell's pass probability — the pooled-
   limit trap #1424 documents for the Hurst gate. Writing a repo-tracked output from a run
   whose universe is not a SUPERSET of the default universe is refused outright
   (``--allow-narrowed-family`` overrides, and warns loudly either way).
1b. BASELINE SOURCES (review). family_is_superset compares BARE symbols, so it cannot see
   a DEFAULT symbol repointed at another venue. baseline_source_violations checks that
   separately: a default symbol must resolve to eval_windows.PLATFORM, because every
   committed regime baseline was computed on that series (#1315). ADDED symbols stay free
   to carry any ``@exchange``. Same refusal, same override flag.
1c. ONE ASSET, ONE SERIES (review). certify() keys a cell by normalize_cert_asset, so two
   screened symbols reducing to the same asset would blend into ONE certified entry whose
   provenance ``criteria.data_sources`` (keyed by full symbol) cannot disentangle.
   Refused at parse time; premise.parse_symbols_arg separately refuses a repeated symbol.
1d. DEGENERATE RUNS (review). An empty ``certified`` list is a publishable negative result
   only when the run COULD have found something. A run with no directional rows measured
   nothing, and a run whose permutation p-floor sits above the rank-1 BH critical value
   cannot reject at any effect size; either one is refused for repo-tracked outputs
   (``--allow-degenerate-run`` — deliberately a SEPARATE flag, so unlocking a narrowed
   research family never also unlocks erasing the live artifact). ``--n-perm`` therefore
   defaults to 30000, the resolution the committed artifact was produced at.
2. PROVENANCE. The artifact ``criteria`` records the actual screened family size, the
   per-symbol data sources, the universe axes and the permutation count, so a narrowed or
   mis-sourced artifact is detectable by inspection after the fact. All new metadata nests
   INSIDE ``criteria`` on purpose: the Go loader parses with DisallowUnknownFields
   (scheduler/regime_directional_certification.go), where ``Criteria`` is a free-form map
   and any NEW top-level key would make the live daemon fail closed on a valid artifact.
   ``schema_version`` stays 1.

The per-cell verdict surface (cell_verdicts) reports, for every screened cell, the FIRST
criterion it fails — so "why is (ETH, 1h, composite) not certified?" is answered by the run
itself instead of being inferred from an empty ``certified`` list. Its ``bh_threshold`` is
the bar the VERDICT was decided against: BH is a step-up procedure, so once the family
rejects anything the operative bar is the step-up cutoff, not each row's per-rank critical
value (both are reported, as ``bh_step_up_cutoff`` and ``bh_rank_threshold``).
"""
from __future__ import annotations

import argparse
import json
import os
import sys
from datetime import datetime, timedelta, timezone

_THIS_DIR = os.path.dirname(os.path.abspath(__file__))
_BACKTEST = os.path.abspath(os.path.join(_THIS_DIR, ".."))
_ROOT = os.path.abspath(os.path.join(_BACKTEST, ".."))
for _p in (_BACKTEST, _ROOT, os.path.join(_ROOT, "shared_tools")):
    if _p not in sys.path:
        sys.path.insert(0, _p)

from regime_stats import benjamini_hochberg  # noqa: E402
from directional_certification import normalize_cert_asset  # noqa: E402
import regime_1076_directional_premise as premise  # noqa: E402

DEFAULT_ARTIFACT = os.path.join(_THIS_DIR, "regime_directional_certifications.json")
DEFAULT_RUN_REPORT = os.path.join(_THIS_DIR, "regime_1443_run_report.json")
DEFAULT_TTL_DAYS = 90
HELD_OUT_FORWARD = premise.HELD_OUT_FORWARD


def _canonical_trend_label(state: str) -> str:
    """Map a (possibly composite sub-) state to the canonical policy label the
    live regime_directional_policy keys on."""
    if state.startswith("trending_up"):
        return "trending_up"
    if state.startswith("trending_down"):
        return "trending_down"
    return state


def _policy_direction_label(policy_dir: int) -> str:
    return "long" if policy_dir > 0 else "short"


def _bare_symbols(symbols) -> set:
    """Bare symbol names from either ``"SYM"`` strings, ``"SYM@exchange"`` specs
    or ``(symbol, exchange)`` pairs."""
    out = set()
    for entry in symbols:
        if isinstance(entry, str):
            out.add(premise.parse_symbol_spec(entry)[0])
        else:
            out.add(str(entry[0]))
    return out


def family_is_superset(symbols, timeframes, windows, classifiers,
                       horizons=None) -> bool:
    """True when this run's universe covers the whole default screen.

    certify() corrects across the rows of ONE invocation, so the BH family IS the
    run's universe. A run that drops any default axis value produces a smaller
    family, and a smaller family resolves a smaller effect — exactly the
    pooled-limit trap #1424 records. Certifying a cell from such a run would let
    a weaker result clear a bar the full screen never lowered. ``horizons`` is
    checked when supplied: dropping horizons shrinks the family just as dropping
    symbols does. Pure; unit-tested without data access."""
    ok = (_bare_symbols(symbols) >= set(premise.DEFAULT_SYMBOLS)
          and set(timeframes) >= set(premise.DEFAULT_TIMEFRAMES)
          and set(windows) >= set(premise.DEFAULT_WINDOWS)
          and set(classifiers) >= set(premise.DEFAULT_CLASSIFIERS))
    if horizons is not None:
        ok = ok and set(int(h) for h in horizons) >= set(premise.DEFAULT_HORIZONS)
    return ok


def baseline_source_violations(sources, platform) -> dict:
    """``{symbol: exchange}`` for DEFAULT_SYMBOLS entries NOT loaded from the
    baseline venue.

    #1443 review: family_is_superset compares BARE symbols on purpose (a default
    symbol still covers its axis value whatever venue it came from), so the width
    check alone cannot see a repointed baseline. It has to be checked separately,
    because the repo artifact is what the live daemon reads to enable
    regime_directional_policy short entries, and every committed regime baseline
    was computed on the ``eval_windows.PLATFORM`` series (#1315 axis separation).
    Symbols ADDED beyond the defaults are free to carry any source — that is the
    whole point of ``SYMBOL[@exchange]``. Pure; unit-tested without data access."""
    return {sym: src for sym, src in sorted(sources.items())
            if sym in set(premise.DEFAULT_SYMBOLS) and src != platform}


def cert_asset_collisions(symbols) -> dict:
    """``{cert_asset: [symbol, ...]}`` for normalized assets backed by more than
    one screened symbol.

    #1443 review: certify() keys a cell by ``normalize_cert_asset(symbol)``, so
    ``BTC/USDT`` and ``BTC/USDC:USDC@hyperliquid`` collapse into ONE certified
    entry whose rows come from two venues, while ``criteria.data_sources`` (keyed
    by full symbol) cannot say which venue backed which state. Screening the same
    asset from two venues in one run is therefore refused at parse time rather
    than silently blended. Pure; unit-tested without data access."""
    by_asset: dict = {}
    for symbol, _exchange in premise.normalize_symbol_specs(symbols):
        by_asset.setdefault(normalize_cert_asset(symbol), []).append(symbol)
    return {asset: sorted(syms) for asset, syms in sorted(by_asset.items())
            if len(syms) > 1}


def _bh_ranks(pvals, fdr_q):
    """``[(rank, critical_value)]`` aligned with ``pvals``: each p-value's 1-based
    ascending rank in the family and the BH critical value ``fdr_q * rank / m``
    it had to clear. Ties take the same (lowest) rank so two identical p-values
    are never reported as passing different bars."""
    m = len(pvals)
    if m == 0:
        return []
    order = sorted(range(m), key=lambda i: pvals[i])
    ranks = [0] * m
    rank = 0
    prev = None
    for pos, i in enumerate(order, start=1):
        if prev is None or pvals[i] > prev:
            rank = pos
            prev = pvals[i]
        ranks[i] = rank
    return [(ranks[i], fdr_q * ranks[i] / m) for i in range(m)]


def bh_step_up_cutoff(pvals, fdr_q):
    """The critical value the family ACTUALLY cleared, or ``None`` when the
    procedure rejected nothing.

    #1443 review: Benjamini-Hochberg is a STEP-UP procedure
    (backtest/regime_stats.py) — it finds the largest rank k with
    ``p_(k) <= q*k/m`` and rejects every p-value at or below ``p_(k)``. So a row
    whose OWN per-rank bar ``q*rank/m`` it missed can still be rejected on the
    back of a higher-ranked survivor. Reporting the per-rank bar as "the bar it
    needed" can therefore contradict the verdict beside it.

    ``q*k_max/m`` is exactly equivalent to the procedure for every row in the
    family: no p-value can lie in ``(p_(k_max), q*k_max/m]`` — one there would
    itself satisfy the rank-``k_max+1`` bar and contradict k_max's maximality.
    Pure; unit-tested without data access."""
    m = len(pvals)
    if m == 0:
        return None
    ordered = sorted(float(p) for p in pvals)
    k_max = 0
    for k, p in enumerate(ordered, start=1):
        if p <= fdr_q * k / m:
            k_max = k
    if not k_max:
        return None
    return fdr_q * k_max / m


# Gate order, mirroring certify(): a cell is reported against the FIRST criterion
# it fails, so the verdict names the binding constraint rather than a downstream one.
VERDICT_NO_DIRECTIONAL_ROWS = "no_directional_rows"
VERDICT_FAILS_GLOBAL_BH = "fails_global_bh"
VERDICT_WRONG_SIGNED = "wrong_signed"
VERDICT_NOT_HELD_OUT = "not_held_out_forward"
VERDICT_CERTIFIED = "certified"

# #1443 review round 2: which tier of rows the displayed `best_row` was drawn
# from. The verdict is decided by a LADDER of nested filters (directional ->
# survived global BH -> sign-aligned -> held-out forward), and each verdict
# names the deepest tier the cell reached. Picking the displayed row from the
# whole directional set let a CERTIFIED cell print evidence that itself reads as
# a miss (`sign_aligned: false`, or a historical window) whenever the cell's
# globally lowest-p row was not one of the rows that actually passed. The row
# shown is therefore always a member of the set the verdict is ABOUT, and this
# field says which set that is so the diagnostic explains its own reasoning.
BASIS_ALL_DIRECTIONAL = "all_directional_rows"
BASIS_SURVIVED_BH = "survived_global_bh"
BASIS_SURVIVED_ALIGNED = "survived_and_aligned"
BASIS_CERTIFIED_HELD_OUT = "certified_held_out_forward"


def cell_verdicts(rows, fdr_q=0.05, held_out_windows=HELD_OUT_FORWARD) -> dict:
    """Per-(asset, timeframe, classifier) verdict over premise-screen rows.

    Reports every SCREENED cell — not only the certified ones — with the first
    certification criterion it fails, its best (minimum) p-value, and the global
    BH critical value that p-value had to clear. The artifact still lists only
    certified cells; this is the run-report surface #1443 requires so a negative
    verdict states WHY. Pure; unit-tested without data access.

    A cell absent from ``rows`` entirely (no window contributed enough bars) does
    not appear here — the caller reports those separately against the requested
    grid."""
    def _key(r):
        return (normalize_cert_asset(r["symbol"]), str(r["timeframe"]),
                str(r["classifier"]).strip().lower())

    n_screened = {}
    for r in rows:
        k = _key(r)
        n_screened[k] = n_screened.get(k, 0) + 1

    # Index-keyed throughout: two rows can compare equal, and a caller may pass the
    # same dict object twice, so positions in the family — never identity — decide
    # which p-value maps to which BH rank.
    directional = [r for r in rows if r.get("policy_dir", 0) != 0]
    pvals = [float(r["p_value"]) for r in directional]
    global_bh = benjamini_hochberg(pvals, alpha=fdr_q) if pvals else []
    ranked = _bh_ranks(pvals, fdr_q)
    step_up = bh_step_up_cutoff(pvals, fdr_q)
    family_size = len(directional)
    cell_idx: dict = {}
    for i, r in enumerate(directional):
        cell_idx.setdefault(_key(r), []).append(i)

    out = {}
    for key in sorted(n_screened):
        idxs = cell_idx.get(key, [])
        entry = {
            "asset": key[0], "timeframe": key[1], "classifier": key[2],
            "n_screened_rows": n_screened[key],
            "n_directional_rows": len(idxs),
            "global_bh_family_size": family_size,
            "fdr_q": fdr_q,
        }
        if not idxs:
            entry["verdict"] = VERDICT_NO_DIRECTIONAL_ROWS
            entry["min_p_value"] = None
            entry["best_row"] = None
            entry["best_row_basis"] = None
            entry["bh_rank"] = None
            entry["bh_threshold"] = None
            entry["bh_rank_threshold"] = None
            entry["bh_step_up_cutoff"] = (
                None if step_up is None else float(step_up))
            out[key] = entry
            continue

        # `min_p_value` and the BH fields stay anchored to the cell's globally
        # lowest-p directional row: that is literally the minimum, and for a cell
        # that failed it is the honest "how far short did it fall". The DISPLAYED
        # row is chosen separately, below, from the tier the verdict names.
        min_i = min(idxs, key=lambda i: pvals[i])
        rank, thresh = ranked[min_i]
        entry["min_p_value"] = float(pvals[min_i])
        entry["bh_rank"] = int(rank)
        # #1443 review: `bh_threshold` is the bar the VERDICT was decided against,
        # so it must be the step-up cutoff whenever the family rejected anything —
        # a row can be rejected on a higher-ranked survivor's back while missing
        # its own per-rank bar. With nothing rejected there is no cutoff, and the
        # row's own per-rank bar is the honest "how far short did it fall".
        entry["bh_rank_threshold"] = float(thresh)
        entry["bh_step_up_cutoff"] = None if step_up is None else float(step_up)
        entry["bh_threshold"] = float(thresh if step_up is None else step_up)
        # Placed here so the emitted key order is stable across the reordering
        # this fix required; the values are filled once the verdict is known.
        entry["best_row"] = None
        entry["best_row_basis"] = None

        passing_idxs = [i for i in idxs if global_bh[i]]
        aligned_idxs = [i for i in passing_idxs
                        if directional[i].get("sign_aligned")]
        held_idxs = [i for i in aligned_idxs
                     if directional[i].get("window") in held_out_windows]
        entry["n_survive_global_bh"] = len(passing_idxs)
        entry["n_survive_and_aligned"] = len(aligned_idxs)
        entry["n_survive_aligned_held_out"] = len(held_idxs)
        if not passing_idxs:
            entry["verdict"] = VERDICT_FAILS_GLOBAL_BH
            evidence_idxs, basis = idxs, BASIS_ALL_DIRECTIONAL
        elif not aligned_idxs:
            entry["verdict"] = VERDICT_WRONG_SIGNED
            evidence_idxs, basis = passing_idxs, BASIS_SURVIVED_BH
        elif not held_idxs:
            entry["verdict"] = VERDICT_NOT_HELD_OUT
            evidence_idxs, basis = aligned_idxs, BASIS_SURVIVED_ALIGNED
        else:
            entry["verdict"] = VERDICT_CERTIFIED
            evidence_idxs, basis = held_idxs, BASIS_CERTIFIED_HELD_OUT
            entry["certified_states"] = dict(sorted(
                (_canonical_trend_label(str(directional[i]["state"])),
                 _policy_direction_label(int(directional[i]["policy_dir"])))
                for i in held_idxs))

        best = directional[min(evidence_idxs, key=lambda i: pvals[i])]
        entry["best_row"] = {
            "window": str(best["window"]), "horizon": int(best["horizon"]),
            "state": str(best["state"]), "gap": float(best["gap"]),
            "p_value": float(best["p_value"]),
            "policy_dir": int(best["policy_dir"]),
            "sign_aligned": bool(best.get("sign_aligned")),
            "source": str(best.get("source", "")),
        }
        entry["best_row_basis"] = basis
        out[key] = entry
    return out


def permutation_p_floor(n_perm: int) -> float:
    """``1/(n_perm+1)`` — the smallest p-value the block-shuffle test can emit
    (regime_diagnostics.per_state_significance). Disclosed next to the rank-1 BH
    critical value ``fdr_q/m`` because when the floor sits ABOVE that value no
    single row can certify at any effect size, however real. The run report must
    say so rather than let an empty artifact read as evidence of absence."""
    n = max(1, int(n_perm))
    return 1.0 / (n + 1)


def certify(rows, fdr_q=0.05, held_out_windows=HELD_OUT_FORWARD,
            ttl_days=DEFAULT_TTL_DAYS, generated_at=None,
            universe=None, data_sources=None, n_perm=None):
    """Pure certification gate over premise-screen rows. Returns the artifact
    dict. Testable without touching data — pass synthetic rows.

    A directional row qualifies iff it survives GLOBAL Benjamini-Hochberg across
    the whole directional family, is sign-aligned, and lands in a held-out
    forward window. Certified cells map each surviving canonical trend label to
    its policy direction.

    ``universe``/``data_sources``/``n_perm`` are optional provenance recorded in
    ``criteria`` (#1443). ``criteria.screened_family_size`` is always emitted and
    is computed FROM THE ROWS, so it reports the family the gate actually
    corrected against — never a claimed one.
    """
    if generated_at is None:
        generated_at = datetime.now(timezone.utc)
    gen_iso = generated_at.replace(microsecond=0).isoformat().replace("+00:00", "Z")
    expires_iso = (
        (generated_at + timedelta(days=ttl_days)).replace(microsecond=0)
        .isoformat().replace("+00:00", "Z")
    )

    directional = [r for r in rows if r.get("policy_dir", 0) != 0]
    pvals = [float(r["p_value"]) for r in directional]
    global_bh = benjamini_hochberg(pvals, alpha=fdr_q) if pvals else []

    # cell -> {canonical_label -> direction}
    cells: dict[tuple, dict] = {}
    for survives, r in zip(global_bh, directional):
        if not survives:
            continue
        if not r.get("sign_aligned"):
            continue
        if r.get("window") not in held_out_windows:
            continue
        key = (normalize_cert_asset(r["symbol"]), str(r["timeframe"]),
               str(r["classifier"]).strip().lower())
        label = _canonical_trend_label(str(r["state"]))
        direction = _policy_direction_label(int(r["policy_dir"]))
        cells.setdefault(key, {})[label] = direction

    # #1443 provenance. Everything nests under `criteria` because the Go loader
    # parses with DisallowUnknownFields and treats `criteria` as a free-form map;
    # a new TOP-LEVEL key would make the live daemon reject a valid artifact.
    criteria = {
        "global_correction": "benjamini-hochberg",
        "fdr_q": fdr_q,
        "also_require_bonferroni": False,
        "require_sign_aligned": True,
        "require_held_out_forward": True,
        "held_out_windows": list(held_out_windows),
        "screened_family_size": len(directional),
    }
    if n_perm is not None:
        criteria["n_perm"] = int(n_perm)
        criteria["permutation_p_floor"] = permutation_p_floor(n_perm)
    if data_sources is not None:
        criteria["data_sources"] = dict(sorted(data_sources.items()))
    if universe is not None:
        criteria["universe"] = {k: list(v) for k, v in sorted(universe.items())}

    certified = []
    for (asset, timeframe, classifier), states in sorted(cells.items()):
        certified.append({
            "asset": asset,
            "timeframe": timeframe,
            "classifier": classifier,
            "generated_at": gen_iso,
            "expires_at": expires_iso,
            "states": dict(sorted(states.items())),
        })

    return {
        "schema_version": 1,
        "generated_at": gen_iso,
        "generator": "backtest/research/regime_1076_certify.py",
        "source_evidence": "backtest/research/README_1076_directional_premise.md",
        "criteria": criteria,
        "default_ttl_days": ttl_days,
        "certified": certified,
    }


def build_parser():
    p = argparse.ArgumentParser(description="#1085 directional-certification producer")
    p.add_argument("--symbols", default=",".join(premise.DEFAULT_SYMBOLS),
                   help="comma-separated SYMBOL[@exchange] specs; a bare symbol uses the "
                        "default data source (#1443). Same contract as the premise script.")
    p.add_argument("--timeframes", default=",".join(premise.DEFAULT_TIMEFRAMES))
    p.add_argument("--windows", default=",".join(premise.DEFAULT_WINDOWS))
    p.add_argument("--horizons", default=",".join(str(h) for h in premise.DEFAULT_HORIZONS))
    p.add_argument("--classifiers", default=",".join(premise.DEFAULT_CLASSIFIERS))
    # #1443 review: the default matches the committed artifact's run. At the full
    # default universe (m in the low thousands) the rank-1 BH bar q/m sits around
    # 4e-05, while n_perm=500 floors every p-value at 1/501 ~ 2e-03 — ~53x ABOVE
    # it. The old default therefore made the documented refresh command incapable
    # of certifying anything at any effect size.
    p.add_argument("--n-perm", type=int, default=30000)
    p.add_argument("--seed", type=int, default=0)
    p.add_argument("--fdr-q", type=float, default=0.05)
    p.add_argument("--ttl-days", type=int, default=DEFAULT_TTL_DAYS)
    p.add_argument("--out", default=DEFAULT_ARTIFACT,
                   help="artifact path to write (default: the repo artifact)")
    p.add_argument("--report-out", default=DEFAULT_RUN_REPORT,
                   help="path for the per-cell run report JSON (empty string to skip)")
    p.add_argument("--allow-narrowed-family", action="store_true",
                   help="permit a run whose universe is NOT a superset of the default "
                        "screen, or whose baseline symbols are sourced off-PLATFORM. "
                        "Narrowing shrinks the BH correction family and inflates every "
                        "cell's pass probability (#1424); a repointed baseline screens a "
                        "series no committed baseline used. Without this flag such a run "
                        "refuses to write ANY repo-tracked output.")
    p.add_argument("--allow-degenerate-run", action="store_true",
                   help="permit a run that is structurally incapable of certifying "
                        "anything — no directional rows screened at all, or a permutation "
                        "p-floor above the rank-1 BH critical value — to overwrite a "
                        "repo-tracked output. Deliberately SEPARATE from "
                        "--allow-narrowed-family: narrowing a research family must not "
                        "also unlock the erasure of a live artifact from a run that "
                        "measured nothing.")
    return p


def _format_verdict_line(v) -> str:
    best = v.get("best_row")
    head = (f"{v['asset']:6s} {v['timeframe']:4s} {v['classifier']:10s} "
            f"{v['verdict']:22s}")
    if not best:
        return head + f"rows={v['n_screened_rows']} directional=0"
    bar_kind = "step-up cutoff" if v.get("bh_step_up_cutoff") is not None \
        else "per-rank bar"
    # The displayed row is the verdict's own evidence, so it is not always the
    # cell minimum. Say so on the line rather than letting `min_p=` be read as
    # the shown row's p-value.
    best_p = ("" if best["p_value"] == v["min_p_value"]
              else f"p={best['p_value']:.6g} ")
    basis = ("" if v.get("best_row_basis") in (None, BASIS_ALL_DIRECTIONAL)
             else f" basis={v['best_row_basis']}")
    return (head
            + f"min_p={v['min_p_value']:.6g} "
            + f"needed<={v['bh_threshold']:.3e} ({bar_kind}; BH rank "
            + f"{v['bh_rank']}/{v['global_bh_family_size']}) "
            + f"best={best['state']}@{best['window']}/h{best['horizon']} "
            + best_p
            + f"gap={best['gap']:+.5f} aligned={'Y' if best['sign_aligned'] else 'N'} "
            + f"src={best['source']}"
            + basis)


def main(argv=None) -> int:
    from eval_windows import WINDOWS, PLATFORM
    from regime import _DEFAULT_COMPOSITE_THRESHOLDS

    args = build_parser().parse_args(argv)
    th = dict(_DEFAULT_COMPOSITE_THRESHOLDS)
    try:
        symbols = premise.parse_symbols_arg(args.symbols)
    except ValueError as exc:
        raise SystemExit(f"--symbols: {exc}")
    timeframes = tuple(t.strip() for t in args.timeframes.split(",") if t.strip())
    windows = tuple(w.strip() for w in args.windows.split(",") if w.strip())
    for w in windows:
        if w not in WINDOWS:
            raise SystemExit(f"unknown window {w}; known: {list(WINDOWS)}")
    horizons = tuple(int(h) for h in args.horizons.split(","))
    classifiers = tuple(c.strip() for c in args.classifiers.split(",") if c.strip())

    # ONE-ASSET-ONE-SERIES GATE (#1443 review). certify() keys a cell by the
    # NORMALIZED asset, so two symbols reducing to the same asset blend into one
    # certified entry whose provenance cannot be told apart. Refuse before any
    # data is touched.
    collisions = cert_asset_collisions(symbols)
    if collisions:
        detail = "; ".join(f"{asset} <- {syms}" for asset, syms in collisions.items())
        raise SystemExit(
            f"--symbols: two screened symbols normalize to the same certification "
            f"asset ({detail}). certify() keys a cell by the normalized asset, so their "
            "rows would blend into ONE certified entry while criteria.data_sources "
            "(keyed by full symbol) could not say which venue backed which state. "
            "Screen one series per asset per run.")

    sources = premise.resolve_data_sources(symbols)

    # REPO-OUTPUT INTEGRITY GATE — before any data is touched, so a refused run
    # costs nothing and can never leave a half-written file behind. EVERY
    # repo-tracked output the producer can write is protected, not just --out:
    # the run report is this issue's committed evidence for the negative result.
    repo_targets = []
    if os.path.abspath(args.out) == os.path.abspath(DEFAULT_ARTIFACT):
        repo_targets.append(args.out)
    if args.report_out and (os.path.abspath(args.report_out)
                            == os.path.abspath(DEFAULT_RUN_REPORT)):
        repo_targets.append(args.report_out)

    integrity_problems = []
    if not family_is_superset(symbols, timeframes, windows, classifiers, horizons):
        integrity_problems.append((
            "narrowed family: this run's universe is NOT a superset of the default "
            "screen (symbols {s} / timeframes {t} / windows {w} / classifiers {c} / "
            "horizons {h}). certify() corrects across the rows of ONE invocation, so a "
            "narrowed run shrinks the Benjamini-Hochberg family and inflates every "
            "cell's pass probability — the pooled-limit trap #1424 records."
        ).format(s=list(premise.DEFAULT_SYMBOLS), t=list(premise.DEFAULT_TIMEFRAMES),
                 w=list(premise.DEFAULT_WINDOWS), c=list(premise.DEFAULT_CLASSIFIERS),
                 h=list(premise.DEFAULT_HORIZONS)))
    repointed = baseline_source_violations(sources, PLATFORM)
    if repointed:
        integrity_problems.append(
            "repointed baseline: default symbols "
            + ", ".join(f"{sym}@{src}" for sym, src in repointed.items())
            + f" are not loaded from the baseline venue {PLATFORM!r}. Every committed "
            "regime baseline was computed on that series (#1315 axis separation), so a "
            "substituted source screens a series no baseline used. Adding NEW symbols "
            "from any venue stays free.")
    if integrity_problems:
        msg = " ALSO: ".join(integrity_problems) + (
            " Re-run over the full default universe on the baseline sources (adding "
            "symbols is fine), or pass --allow-narrowed-family and write elsewhere.")
        if repo_targets and not args.allow_narrowed_family:
            raise SystemExit(
                f"REFUSING to write repo-tracked output {repo_targets}: {msg}")
        print(f"[WARN] {msg}")
    universe = {"symbols": sorted(sources), "timeframes": list(timeframes),
                "windows": list(windows), "horizons": [int(h) for h in horizons],
                "classifiers": list(classifiers)}
    print(f"# certify universe: {sorted(sources)} x {list(timeframes)} x {list(windows)}")
    print("# data sources: "
          + " ".join(f"{sym}={src}" for sym, src in sorted(sources.items())))
    print(f"# classifiers={list(classifiers)} horizons={list(horizons)} "
          f"n_perm={args.n_perm} seed={args.seed} default_platform={PLATFORM}")

    rows = premise.run(symbols, timeframes, windows, horizons, classifiers, th,
                       args.n_perm, args.seed)
    # certify() is pure — nothing is written until the degenerate-run gate below
    # has passed, so a refusal leaves every existing file byte-identical.
    artifact = certify(rows, fdr_q=args.fdr_q, ttl_days=args.ttl_days,
                       universe=universe, data_sources=sources, n_perm=args.n_perm)

    family_size = artifact["criteria"]["screened_family_size"]
    p_floor = permutation_p_floor(args.n_perm)
    rank1 = args.fdr_q / family_size if family_size else float("nan")
    coverage = premise.coverage_table(rows)
    verdicts = cell_verdicts(rows, fdr_q=args.fdr_q)

    print()
    print("SCREENED COVERAGE — (symbol, tf, window) cells that contributed rows:")
    for e in coverage:
        bars = " ".join(f"{c}={n}" for c, n in sorted(e["bars"].items()))
        print(f"  {e['symbol']:18s} {e['source']:12s} {e['timeframe']:4s} "
              f"{e['window']:8s} rows={e['rows']:5d}  bars {bars}")
    present = {(e["symbol"], e["timeframe"], e["window"]) for e in coverage}
    empty = [(sym, tf, w)
             for sym, _ex in premise.normalize_symbol_specs(symbols)
             for tf in timeframes for w in windows
             if (sym, tf, w) not in present]
    if empty:
        print("  windows contributing NO rows (asset not listed yet, or too few bars):")
        for sym, tf, w in empty:
            print(f"    {sym:18s} {tf:4s} {w}")

    print()
    print(f"PERMUTATION RESOLUTION: p-floor 1/(n_perm+1) = {p_floor:.3e} "
          f"(n_perm={args.n_perm}); rank-1 global-BH critical value q/m = "
          f"{rank1:.3e} (q={args.fdr_q}, m={family_size}).")
    if not family_size:
        print("  ** The screen produced NO directional rows at all — there was nothing "
              "to correct. This is a coverage failure, not a negative result. **")
    elif p_floor > rank1:
        print("  ** The floor sits ABOVE the rank-1 bar: no SINGLE row can certify at "
              "this n_perm regardless of effect size. An empty artifact from this run "
              "is NOT evidence of absence. **")
    else:
        print("  The floor sits at or below the rank-1 bar: a single true row is "
              "arithmetically able to certify.")

    print()
    print("PER-CELL VERDICTS (first failing criterion; the artifact lists only "
          "certified cells):")
    for key in sorted(verdicts):
        print("  " + _format_verdict_line(verdicts[key]))

    # DEGENERATE-RUN GATE (#1443 review). An empty ``certified`` list is a
    # publishable negative result ONLY when the run could have found something.
    # A run with no directional rows measured nothing, and a run whose p-floor
    # sits above the rank-1 BH bar cannot reject at any effect size — either one
    # overwriting a repo-tracked file republishes "nothing certified" as evidence
    # and would silently erase a future non-empty certified list.
    degenerate = []
    if not family_size:
        degenerate.append(
            "the screen produced NO directional rows at all, so there was nothing to "
            "correct — a coverage failure (unreachable OHLCV cache, or every window "
            "too short), not a negative result")
    elif p_floor > rank1:
        degenerate.append(
            f"the permutation p-floor {p_floor:.3e} (n_perm={args.n_perm}) sits ABOVE "
            f"the rank-1 global-BH critical value {rank1:.3e} (q={args.fdr_q}, "
            f"m={family_size}), so no single row can certify at any effect size — an "
            "empty artifact from this run is not evidence of absence. Raise --n-perm")
    if degenerate and repo_targets and not args.allow_degenerate_run:
        raise SystemExit(
            f"REFUSING to write repo-tracked output {repo_targets}: "
            + "; ".join(degenerate)
            + ". Pass --allow-degenerate-run to overwrite anyway, or write elsewhere "
              "with --out/--report-out.")
    if degenerate:
        print(f"[WARN] degenerate run: {'; '.join(degenerate)}")

    with open(args.out, "w") as fh:
        json.dump(artifact, fh, indent=2)
        fh.write("\n")

    n = len(artifact["certified"])
    print()
    print(f"# wrote {n} certified cell(s) -> {args.out}")
    if n == 0:
        print("# nothing survived global correction (#1076 negative result) — "
              "all regime_directional_policy strategies run default-off.")

    if args.report_out:
        report = {
            "issue": 1443,
            "generator": "backtest/research/regime_1076_certify.py",
            "generated_at": artifact["generated_at"],
            "universe": universe,
            "data_sources": dict(sorted(sources.items())),
            "seed": args.seed,
            "n_perm": args.n_perm,
            "fdr_q": args.fdr_q,
            "screened_family_size": family_size,
            "permutation_p_floor": p_floor,
            "global_bh_rank1_threshold": rank1,
            "single_row_certifiable": bool(family_size and p_floor <= rank1),
            "coverage": coverage,
            "empty_windows": [{"symbol": s_, "timeframe": t_, "window": w_}
                              for s_, t_, w_ in empty],
            "cell_verdicts": [verdicts[k] for k in sorted(verdicts)],
            "certified": artifact["certified"],
        }
        with open(args.report_out, "w") as fh:
            json.dump(report, fh, indent=2)
            fh.write("\n")
        print(f"# wrote run report ({len(verdicts)} screened cells) -> {args.report_out}")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())

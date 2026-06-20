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
        --symbols BTC/USDT,ETH/USDT --timeframes 1h,4h --classifiers adx,composite
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


def certify(rows, fdr_q=0.05, held_out_windows=HELD_OUT_FORWARD,
            ttl_days=DEFAULT_TTL_DAYS, generated_at=None):
    """Pure certification gate over premise-screen rows. Returns the artifact
    dict. Testable without touching data — pass synthetic rows.

    A directional row qualifies iff it survives GLOBAL Benjamini-Hochberg across
    the whole directional family, is sign-aligned, and lands in a held-out
    forward window. Certified cells map each surviving canonical trend label to
    its policy direction.
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
        "criteria": {
            "global_correction": "benjamini-hochberg",
            "fdr_q": fdr_q,
            "also_require_bonferroni": False,
            "require_sign_aligned": True,
            "require_held_out_forward": True,
            "held_out_windows": list(held_out_windows),
        },
        "default_ttl_days": ttl_days,
        "certified": certified,
    }


def build_parser():
    p = argparse.ArgumentParser(description="#1085 directional-certification producer")
    p.add_argument("--symbols", default=",".join(premise.DEFAULT_SYMBOLS))
    p.add_argument("--timeframes", default=",".join(premise.DEFAULT_TIMEFRAMES))
    p.add_argument("--windows", default=",".join(premise.DEFAULT_WINDOWS))
    p.add_argument("--horizons", default=",".join(str(h) for h in premise.DEFAULT_HORIZONS))
    p.add_argument("--classifiers", default=",".join(premise.DEFAULT_CLASSIFIERS))
    p.add_argument("--n-perm", type=int, default=500)
    p.add_argument("--seed", type=int, default=0)
    p.add_argument("--fdr-q", type=float, default=0.05)
    p.add_argument("--ttl-days", type=int, default=DEFAULT_TTL_DAYS)
    p.add_argument("--out", default=DEFAULT_ARTIFACT,
                   help="artifact path to write (default: the repo artifact)")
    return p


def main(argv=None) -> int:
    from eval_windows import WINDOWS, PLATFORM
    from regime import _DEFAULT_COMPOSITE_THRESHOLDS

    args = build_parser().parse_args(argv)
    th = dict(_DEFAULT_COMPOSITE_THRESHOLDS)
    symbols = tuple(s.strip() for s in args.symbols.split(",") if s.strip())
    timeframes = tuple(t.strip() for t in args.timeframes.split(",") if t.strip())
    windows = tuple(w.strip() for w in args.windows.split(",") if w.strip())
    for w in windows:
        if w not in WINDOWS:
            raise SystemExit(f"unknown window {w}; known: {list(WINDOWS)}")
    horizons = tuple(int(h) for h in args.horizons.split(","))
    classifiers = tuple(c.strip() for c in args.classifiers.split(",") if c.strip())

    print(f"# certify universe: {list(symbols)} x {list(timeframes)} x {list(windows)}")
    print(f"# classifiers={list(classifiers)} n_perm={args.n_perm} platform={PLATFORM}")
    rows = premise.run(symbols, timeframes, windows, horizons, classifiers, th,
                       args.n_perm, args.seed)
    artifact = certify(rows, fdr_q=args.fdr_q, ttl_days=args.ttl_days)
    with open(args.out, "w") as fh:
        json.dump(artifact, fh, indent=2)
        fh.write("\n")
    n = len(artifact["certified"])
    print(f"# wrote {n} certified cell(s) -> {args.out}")
    if n == 0:
        print("# nothing survived global correction (#1076 negative result) — "
              "all regime_directional_policy strategies run default-off.")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())

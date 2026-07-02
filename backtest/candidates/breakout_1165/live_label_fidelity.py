"""Live-label fidelity gate for wiring `comp_up_clean_p21` live (#1197).

The #1165 evidence scored composite `trending_up_clean` labels (period 21)
computed over the FULL cached history of each dataset. The live gate reads
labels from `shared_scripts/check_regime.py`, which recomputes the composite
over a BOUNDED fetch (`--ohlcv-limit`, default 200 bars) each cycle — the
composite's ADX sub-recursion is seeded only ~200 bars back, so the same
calendar bar can label differently live vs in the evidence (#1082). Before the
gate goes live, this driver measures that drift: per-bar hand-rule composite
labels, full-window vs bounded-window, via the same
`regime_bounded_window_validate` arms the #1074 promotion gate uses (we call
the same functions live calls; nothing is re-implemented here).

Promotion criterion (the #1082 bar applied to a hand-rule gate — there is no
fitted model, so `gate_verdict`/provenance don't apply): for EVERY dataset the
live strategy could trade (the same six the evidence covered) and every
protocol window, full-vs-bounded label agreement >= 0.95 measured on >= 30
bars both views scored. Fail closed: a missing/short window blocks, it never
vacuously passes. The artifact also reports the binary agreement on
`trending_up_clean` membership — the exact bit `allowed_regimes` consumes —
which can never be lower than the all-label agreement it is gated on.

Run from the repo root with the cache DB present:

    uv run --no-sync python backtest/candidates/breakout_1165/live_label_fidelity.py \
        --json backtest/candidates/breakout_1165/live_label_fidelity.json

Exit code is non-zero when ANY row blocks, so the #1197 wiring decision can
never ride a partially-failed run.
"""
from __future__ import annotations

import os
import sys

_THIS_DIR = os.path.dirname(os.path.abspath(__file__))
_BACKTEST = os.path.abspath(os.path.join(_THIS_DIR, "..", ".."))
_ROOT = os.path.abspath(os.path.join(_BACKTEST, ".."))
for _p in (_BACKTEST, os.path.join(_ROOT, "shared_tools")):
    if _p not in sys.path:
        sys.path.insert(0, _p)

# The gate under promotion: composite `trending_up_clean` at period 21
# (comp_up_clean_p21, README step 2 plateau peak). The thresholds stay the
# shared defaults — the evidence ran them and the live window spec will too.
GATE_PERIOD = 21
GATE_LABEL = "trending_up_clean"
PROTOCOL_WINDOWS = ("is", "oos")


def gate_membership_flips(label_drift: dict, gate_label: str = GATE_LABEL) -> int:
    """Count of disagreeing bars where the two views differ ON THE GATE BIT
    (exactly one side labels `gate_label`). `label_drift` is a
    `label_drift_stats` dict; its `transitions` maps "full->bounded" pairs to
    counts for every disagreeing bar, so the gate-relevant subset is the pairs
    where membership differs. A "clean->choppy" flip blocks an entry the
    evidence took; "choppy->clean" admits one it never scored — both count.
    """
    flips = 0
    for key, count in (label_drift.get("transitions") or {}).items():
        full_label, _, bounded_label = key.partition("->")
        if (full_label == gate_label) != (bounded_label == gate_label):
            flips += int(count)
    return flips


def row_verdict(label_drift: dict, *, agreement_threshold: float,
                min_agreement_bars: int) -> dict:
    """Fail-closed pass/block for one (dataset, window) row, mirroring the
    #1082 `go_no_go` agreement arm: enough comparable bars first (a short
    window yields a vacuous agreement=1.0 on ~0 bars and must BLOCK), then the
    all-label agreement threshold. The binary gate-membership agreement is
    derived and reported but not separately gated — disagreements on the gate
    bit are a subset of all-label disagreements, so it can never be lower.
    """
    n = int(label_drift.get("n", 0))
    agreement = float(label_drift.get("agreement", 0.0))
    flips = gate_membership_flips(label_drift)
    gate_agreement = 1.0 if n == 0 else 1.0 - flips / n
    reasons: list[str] = []
    if n < min_agreement_bars:
        reasons.append(
            f"insufficient comparable bars: {n} < {min_agreement_bars} "
            "(agreement not measurable -> fail closed)")
    elif agreement < agreement_threshold:
        reasons.append(
            f"full-vs-bounded label agreement {agreement:.4f} "
            f"< threshold {agreement_threshold:.4f}")
    return {
        "n": n,
        "agreement": agreement,
        "gate_membership_flips": flips,
        "gate_membership_agreement": gate_agreement,
        "passes": not reasons,
        "blocking_reasons": reasons,
    }


def overall_verdict(rows: list, expected_rows: int) -> dict:
    """Promote iff every expected row exists and passes. A run that produced
    fewer rows than the dataset x window matrix (loader error, empty cache)
    blocks — silence is not evidence."""
    reasons: list[str] = []
    if len(rows) < expected_rows:
        reasons.append(
            f"only {len(rows)}/{expected_rows} dataset-window rows produced "
            "(missing rows fail closed)")
    for row in rows:
        if not row["verdict"]["passes"]:
            reasons.append(
                f"{row['symbol']} {row['timeframe']} {row['window']}: "
                + "; ".join(row["verdict"]["blocking_reasons"]))
    return {"promote": not reasons, "blocking_reasons": reasons}


def main(argv=None) -> int:
    import argparse
    import json

    from eval_windows import DATASETS
    from regime_bounded_window_validate import (
        DEFAULT_AGREEMENT_THRESHOLD,
        DEFAULT_LOOKBACK,
        DEFAULT_MIN_AGREEMENT_BARS,
        validate,
    )

    parser = argparse.ArgumentParser(
        description="Full-vs-bounded composite label fidelity at the gate "
                    "period (#1197 wiring gate for comp_up_clean_p21)")
    parser.add_argument("--period", type=int, default=GATE_PERIOD)
    parser.add_argument("--lookback", type=int, default=DEFAULT_LOOKBACK,
                        help=f"live bounded fetch size (default {DEFAULT_LOOKBACK}, "
                             "mirrors check_regime --ohlcv-limit)")
    parser.add_argument("--windows", default=",".join(PROTOCOL_WINDOWS),
                        help="comma list of eval_windows keys "
                             f"(default {','.join(PROTOCOL_WINDOWS)})")
    parser.add_argument("--agreement-threshold", type=float,
                        default=DEFAULT_AGREEMENT_THRESHOLD)
    parser.add_argument("--min-agreement-bars", type=int,
                        default=DEFAULT_MIN_AGREEMENT_BARS)
    parser.add_argument("--json", default=None,
                        help="write the full report JSON to this path")
    args = parser.parse_args(argv)

    windows = [w.strip() for w in args.windows.split(",") if w.strip()]
    rows = []
    for symbol, timeframe in DATASETS:
        for window in windows:
            rep = validate(symbol, timeframe, window, None,
                           lookback=args.lookback,
                           incumbent_period=args.period)
            hr = rep["handrule"]
            rows.append({
                "symbol": symbol,
                "timeframe": timeframe,
                "window": window,
                "n_eval_bars": rep["n_eval_bars"],
                "label_drift": hr["label_drift"],
                "adx_drift": rep["adx_drift"],
                "verdict": row_verdict(
                    hr["label_drift"],
                    agreement_threshold=args.agreement_threshold,
                    min_agreement_bars=args.min_agreement_bars),
            })
    payload = {
        "gate": {"label": GATE_LABEL, "period": int(args.period),
                 "lookback": int(args.lookback),
                 "agreement_threshold": float(args.agreement_threshold),
                 "min_agreement_bars": int(args.min_agreement_bars)},
        "windows": windows,
        "rows": rows,
        "verdict": overall_verdict(rows, len(DATASETS) * len(windows)),
    }
    text = json.dumps(payload, indent=2, default=float)
    if args.json:
        with open(args.json, "w") as fh:
            fh.write(text)
    print(text)
    return 0 if payload["verdict"]["promote"] else 1


if __name__ == "__main__":
    raise SystemExit(main())

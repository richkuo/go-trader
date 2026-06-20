"""Scope-2 (economic, isolation form) evidence for #1076: does choosing trade SIDE by the
current regime label earn risk-adjusted PnL the regime-agnostic base does not?

``regime_directional_policy`` (#779) overrides a live strategy's long/short side from the
current regime label. Scope 1 (regime_1076_directional_premise.py) screens whether the label
*statistically* separates forward returns. This is the economic complement: a look-ahead-safe
regime-timing portfolio that prices the bare directional premise with no strategy-signal
confound.

Three always-in-market (or in/flat) books on identical bars, each side decided from the
regime label known at the PRIOR bar close (mirrors the backtester's regime shift(1), #730):

  policy      long in trending_up*, SHORT in trending_down*, flat (or long) in ranging*
  long_only   long in trending_up*, flat otherwise        (isolates "short the downtrend" value)
  buyhold     long every bar                              (regime-agnostic base)

If `policy` does not beat `buyhold`/`long_only` on BOTH Sharpe and DDadj on the held-out
forward windows (is/oos, 2025-06->2026), the regime->direction premise has no economic value
to confer. Shorting funding cost is omitted, which FAVORS `policy` — so a `policy` loss here
is conservative. Fees are charged on turnover (taker bps per unit side change).

This is the transparent isolation; the live-faithful confirmation runs the actual Backtester +
regime_directional_policy config (separate harness). Read-only; no live/Go path touched.

    uv run --no-sync python backtest/research/regime_1076_economic_sim.py
    uv run --no-sync python backtest/research/regime_1076_economic_sim.py \
        --symbols BTC/USDT --timeframes 1h --classifiers adx --ranging-mode long
"""
from __future__ import annotations
import os
import sys

_THIS_DIR = os.path.dirname(os.path.abspath(__file__))
_BACKTEST = os.path.abspath(os.path.join(_THIS_DIR, ".."))
_ROOT = os.path.abspath(os.path.join(_BACKTEST, ".."))
for _p in (_BACKTEST, _ROOT, os.path.join(_ROOT, "shared_tools")):
    if _p not in sys.path:
        sys.path.insert(0, _p)

import numpy as np

from regime import (compute_regime, compute_regime_composite,
                    composite_feature_matrix, _DEFAULT_COMPOSITE_THRESHOLDS)
from data_fetcher import load_cached_data
from eval_windows import WINDOWS, PLATFORM, dd_adjusted_return
from regime_stats import benjamini_hochberg

DEFAULT_SYMBOLS = ("BTC/USDT", "ETH/USDT", "SOL/USDT")
DEFAULT_TIMEFRAMES = ("1h", "4h")
DEFAULT_WINDOWS = ("is", "oos", "2023", "2024", "2025H1")
DEFAULT_CLASSIFIERS = ("adx", "composite")
HELD_OUT_FORWARD = ("is", "oos")
COMPOSITE_PERIOD = 48
ADX_PERIOD = 14
ADX_THRESHOLD = 20.0
BARS_PER_YEAR = {"15m": 4 * 24 * 365, "30m": 2 * 24 * 365, "1h": 24 * 365,
                 "2h": 12 * 365, "4h": 6 * 365, "1d": 365}


def _policy_side(label: str, ranging_mode: str) -> int:
    if label.startswith("trending_up"):
        return +1
    if label.startswith("trending_down"):
        return -1
    return +1 if ranging_mode == "long" else 0


def _labels(df, classifier, th):
    if classifier == "composite":
        labels = compute_regime_composite(df, period=COMPOSITE_PERIOD,
                                          thresholds=th)["regime"].to_numpy()
        feats = composite_feature_matrix(df, COMPOSITE_PERIOD, th).to_numpy()
        valid = ~np.isnan(feats).any(axis=1)
    else:
        labels = compute_regime(df, period=ADX_PERIOD,
                                adx_threshold=ADX_THRESHOLD)["regime"].to_numpy()
        valid = np.ones(len(labels), dtype=bool)
        valid[:ADX_PERIOD] = False
    return labels, valid


def _book(close, decision_side, fee_rate):
    """Equity metrics for a book. ``decision_side[t]`` is the side chosen at the CLOSE of
    bar t from the regime known at bar t (labels[t]); it is held over the next move
    close[t] -> close[t+1]. Mirrors the backtester's "decide at N, fill the N->N+1 move"
    convention (#730) so the position never sees the return it is about to capture — the
    look-ahead-free alignment that makes this consistent with the scope-1 forward-return
    test. (Using decision_side[1:] here instead would let labels[t+1], which is computed
    from close[t+1], pick the side for the t->t+1 move = look-ahead, and inflates Sharpe to
    physically impossible levels.)"""
    ret = close[1:] / close[:-1] - 1.0           # move t->t+1, indexed t in 0..N-2
    pos = decision_side[:-1]                      # side decided at bar t, held over that move
    prev = np.concatenate([[0.0], pos[:-1]])     # side held over the PRIOR move (flat at t=0)
    gross = pos * ret
    turnover = np.abs(pos - prev)                # unit side change entering the move at t
    net = gross - turnover * fee_rate
    eq = np.cumprod(1.0 + net)
    if len(eq) == 0:
        return None
    total_ret = float(eq[-1] - 1.0) * 100.0
    peak = np.maximum.accumulate(eq)
    max_dd = float(np.min(eq / peak - 1.0)) * 100.0
    mu, sd = float(np.mean(net)), float(np.std(net))
    return {
        "total_return_pct": total_ret,
        "max_drawdown_pct": max_dd,
        "sharpe": 0.0, "_mu": mu, "_sd": sd,
        "ddadj": round(dd_adjusted_return(total_ret, max_dd), 3),
        "exposure": float(np.mean(pos != 0)),
        "n_flips": int(np.sum(turnover > 0)),
    }


def _annualize_sharpe(book, timeframe):
    if book is None or book["_sd"] == 0:
        return 0.0
    bpy = BARS_PER_YEAR.get(timeframe, 365)
    return round(book["_mu"] / book["_sd"] * np.sqrt(bpy), 3)


def _sides(labels, valid, ranging_mode):
    """Per-bar DECISION side arrays: entry[t] is the side chosen at bar t's close from
    labels[t]; _book holds it over the t->t+1 move (the 1-bar lag lives in _book). A
    warmup/invalid bar is forced flat for all books."""
    n = len(labels)
    pol = np.zeros(n); lon = np.zeros(n); buy = np.zeros(n)
    for i in range(n):
        if not valid[i]:
            continue
        lab = str(labels[i])
        pol[i] = _policy_side(lab, ranging_mode)
        lon[i] = 1 if lab.startswith("trending_up") else 0
        buy[i] = 1
    return pol, lon, buy


def _mean_dwell(side):
    """Average run length of the side series (its persistence / dwell)."""
    n = len(side)
    if n < 2:
        return 1.0
    boundaries = int(np.count_nonzero(side[1:] != side[:-1])) + 1
    return n / boundaries


def _block_shuffle(arr, block_len, rng):
    n = len(arr)
    block_len = max(1, int(block_len))
    starts = list(range(0, n, block_len))
    perm = rng.permutation(len(starts))
    out = np.concatenate([arr[s:s + block_len] for s in (starts[i] for i in perm)])
    return out[:n]


def _placebo_pvalue(close, decision_side, timeframe, fee_rate, n_perm, seed):
    """Block-shuffle the policy's per-bar SIDE decisions (preserving the long/short/flat mix
    and dwell, destroying the alignment with price) and ask how often the shuffled book's
    Sharpe matches/beats the real one. Small p => the regime label's TIMING carries economic
    value beyond its marginal exposure; large p => the apparent edge is just the exposure mix
    (e.g. defensive beta in a down sample), not regime->direction skill."""
    real = _book(close, decision_side, fee_rate)
    if real is None:
        return None, None
    real_sharpe = _annualize_sharpe(real, timeframe)
    bl = max(int(3 * _mean_dwell(decision_side)), 1)
    rng = np.random.default_rng(seed)
    ge = 0
    for _ in range(n_perm):
        shuf = _block_shuffle(decision_side, bl, rng)
        b = _book(close, shuf, fee_rate)
        if b is not None and _annualize_sharpe(b, timeframe) >= real_sharpe:
            ge += 1
    return float((ge + 1) / (n_perm + 1)), real_sharpe


def run(symbols, timeframes, windows, classifiers, th, ranging_mode, fee_rate,
        placebo_perm=0, seed=0):
    rows = []
    for classifier in classifiers:
        for symbol in symbols:
            for timeframe in timeframes:
                for window in windows:
                    start, end = WINDOWS[window]
                    df = load_cached_data(symbol, timeframe, exchange_id=PLATFORM,
                                          start_date=start, end_date=end)
                    if len(df) <= max(COMPOSITE_PERIOD, ADX_PERIOD) + 5:
                        continue
                    close = df["close"].to_numpy()
                    labels, valid = _labels(df, classifier, th)
                    pol_s, lon_s, buy_s = _sides(labels, valid, ranging_mode)
                    books = {}
                    for name, side in (("policy", pol_s), ("long_only", lon_s),
                                       ("buyhold", buy_s)):
                        b = _book(close, side, fee_rate)
                        if b is not None:
                            b["sharpe"] = _annualize_sharpe(b, timeframe)
                        books[name] = b
                    if books["policy"] is None or books["buyhold"] is None:
                        continue
                    p, bh, lo = books["policy"], books["buyhold"], books["long_only"]
                    placebo_p = None
                    if placebo_perm > 0:
                        placebo_p, _ = _placebo_pvalue(close, pol_s, timeframe, fee_rate,
                                                       placebo_perm, seed)
                    rows.append({
                        "classifier": classifier, "symbol": symbol, "timeframe": timeframe,
                        "window": window,
                        "policy": p, "long_only": lo, "buyhold": bh,
                        "beats_buyhold": bool(p["sharpe"] > bh["sharpe"]
                                              and p["ddadj"] > bh["ddadj"]),
                        "beats_long_only": bool(lo is not None
                                                and p["sharpe"] > lo["sharpe"]
                                                and p["ddadj"] > lo["ddadj"]),
                        "placebo_p": placebo_p,
                    })
    return rows


def report(rows, ranging_mode, fee_rate):
    print("=" * 96)
    print(f"REGIME-TIMING ECONOMIC ISOLATION (#1076 scope 2) | ranging={ranging_mode} "
          f"fee={fee_rate*1e4:.0f}bps/side")
    print("=" * 96)
    hdr = (f"{'clf':9s} {'sym':9s} {'tf':4s} {'win':7s} | "
           f"{'pol_shrp':>8s} {'pol_ret':>8s} {'pol_dda':>8s} | "
           f"{'bh_shrp':>8s} {'bh_ret':>8s} | {'>BH':>4s} {'>LO':>4s}")
    print(hdr); print("-" * len(hdr))
    for r in rows:
        p, bh = r["policy"], r["buyhold"]
        print(f"{r['classifier']:9s} {r['symbol']:9s} {r['timeframe']:4s} {r['window']:7s} | "
              f"{p['sharpe']:8.2f} {p['total_return_pct']:8.1f} {p['ddadj']:8.2f} | "
              f"{bh['sharpe']:8.2f} {bh['total_return_pct']:8.1f} | "
              f"{'Y' if r['beats_buyhold'] else '.':>4s} "
              f"{'Y' if r['beats_long_only'] else '.':>4s}")

    held = [r for r in rows if r["window"] in HELD_OUT_FORWARD]
    oos = [r for r in rows if r["window"] == "oos"]
    print()
    print("SUMMARY — policy beats regime-agnostic base on BOTH Sharpe AND DDadj:")
    print(f"  all cells:           beats buyhold {sum(r['beats_buyhold'] for r in rows):3d}"
          f"/{len(rows):<3d}   beats long_only {sum(r['beats_long_only'] for r in rows):3d}"
          f"/{len(rows)}")
    print(f"  held-out (is/oos):   beats buyhold {sum(r['beats_buyhold'] for r in held):3d}"
          f"/{len(held):<3d}   beats long_only {sum(r['beats_long_only'] for r in held):3d}"
          f"/{len(held)}")
    print(f"  oos (2026-) only:    beats buyhold {sum(r['beats_buyhold'] for r in oos):3d}"
          f"/{len(oos):<3d}   beats long_only {sum(r['beats_long_only'] for r in oos):3d}"
          f"/{len(oos)}")
    print()

    # Placebo control: does the regime label's TIMING add economic value over its own
    # block-shuffled null (same long/short/flat mix + dwell, randomized alignment)? Without
    # this, "beats buyhold" in a down sample is indistinguishable from defensive beta.
    placebo_rows = [r for r in rows if r.get("placebo_p") is not None]
    if placebo_rows:
        pvals = [r["placebo_p"] for r in placebo_rows]
        rej = benjamini_hochberg(pvals, alpha=0.05)
        n_raw = sum(p <= 0.05 for p in pvals)
        n_bh = sum(rej)
        print("PLACEBO CONTROL — real policy Sharpe vs its block-shuffled-label null:")
        print(f"  cells where regime TIMING beats shuffled null (raw p<=0.05):     "
              f"{n_raw:3d}/{len(placebo_rows)}")
        print(f"  ... surviving Benjamini-Hochberg FDR q=0.05 across cells:        "
              f"{n_bh:3d}/{len(placebo_rows)}")
        survivors = [(r, p) for r, p, b in zip(placebo_rows, pvals, rej) if b]
        for r, p in sorted(survivors, key=lambda x: x[1]):
            print(f"    {r['classifier']:9s} {r['symbol']:9s} {r['timeframe']:4s} "
                  f"{r['window']:7s}  placebo_p={p:.3f}  pol_sharpe={r['policy']['sharpe']:.2f}")
        if n_bh == 0:
            print("  => no cell's regime timing beats its own shuffled null after FDR: the")
            print("     economic 'wins' are the exposure mix (defensive beta), NOT regime")
            print("     direction skill. The premise has no economic edge to confer.")
        print()


def build_parser():
    import argparse
    p = argparse.ArgumentParser(description="#1076 scope-2: regime-timing economic isolation")
    p.add_argument("--symbols", default=",".join(DEFAULT_SYMBOLS))
    p.add_argument("--timeframes", default=",".join(DEFAULT_TIMEFRAMES))
    p.add_argument("--windows", default=",".join(DEFAULT_WINDOWS))
    p.add_argument("--classifiers", default=",".join(DEFAULT_CLASSIFIERS))
    p.add_argument("--ranging-mode", default="flat", choices=("flat", "long"),
                   help="side in ranging regimes: flat (default) or long")
    p.add_argument("--fee-bps", type=float, default=10.0, help="taker fee bps per unit turnover")
    p.add_argument("--placebo-perm", type=int, default=0,
                   help="block-shuffle placebo permutations per cell (0=off; 300 for rigor)")
    p.add_argument("--seed", type=int, default=0)
    p.add_argument("--out", default="")
    return p


def main(argv=None) -> int:
    args = build_parser().parse_args(argv)
    th = dict(_DEFAULT_COMPOSITE_THRESHOLDS)
    symbols = tuple(s.strip() for s in args.symbols.split(",") if s.strip())
    timeframes = tuple(t.strip() for t in args.timeframes.split(",") if t.strip())
    windows = tuple(w.strip() for w in args.windows.split(",") if w.strip())
    for w in windows:
        if w not in WINDOWS:
            raise SystemExit(f"unknown window {w}; known: {list(WINDOWS)}")
    classifiers = tuple(c.strip() for c in args.classifiers.split(",") if c.strip())
    fee_rate = args.fee_bps / 1e4

    print(f"# universe: {list(symbols)} x {list(timeframes)} x {list(windows)} "
          f"classifiers={list(classifiers)} platform={PLATFORM}\n")
    rows = run(symbols, timeframes, windows, classifiers, th, args.ranging_mode, fee_rate,
               placebo_perm=args.placebo_perm, seed=args.seed)
    report(rows, args.ranging_mode, fee_rate)
    if args.out:
        import json
        with open(args.out, "w") as fh:
            json.dump({"ranging_mode": args.ranging_mode, "fee_bps": args.fee_bps,
                       "placebo_perm": args.placebo_perm, "rows": rows}, fh, indent=2)
        print(f"# wrote {len(rows)} rows -> {args.out}")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())

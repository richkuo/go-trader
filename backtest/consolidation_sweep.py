
import argparse
import datetime
import itertools
import os
import sys

sys.path.insert(0, os.path.dirname(__file__))

import consolidation_research as cr

DEFAULTS = {
    "min_bars": 12,
    "box_width_pct": 0.02,
    "bandwidth_threshold": 0.7,
    "flatness_slope": 0.0006,
    "flatness_residual": 0.02,
    "escape_k": 1.5,
    "atr_period": 14,
}

PHASE_GRIDS = {
    1: (
        "range_containment",
        [
            {"box_width_pct": bwp, "min_bars": mb}
            for bwp, mb in itertools.product(
                [0.015, 0.02, 0.025, 0.03, 0.04],
                [8, 12, 16, 24, 36],
            )
        ],
    ),
    2: (
        "range_containment",
        [{"escape_k": k} for k in [1.0, 1.25, 1.5, 2.0, 2.5]]
        + [{"atr_period": p} for p in [7, 14, 21]],
    ),
    3: (
        "range_containment",
        [
            {"box_width_pct": bwp, "min_bars": mb}
            for bwp, mb in itertools.product(
                [0.01, 0.015, 0.02, 0.03, 0.05],
                [12, 16, 24],
            )
        ],
    ),
}


def _grid_for(phase, base):
    if phase in PHASE_GRIDS:
        return PHASE_GRIDS[phase]
    raise SystemExit(f"unknown phase {phase}; available: {sorted(PHASE_GRIDS)}")


def _run_phase4(args):
    from data_fetcher import fetch_full_history

    method = "range_containment"
    params = dict(DEFAULTS)
    symbols = [s.strip() for s in args.symbols.split(",") if s.strip()]
    run_date = datetime.date.today().isoformat()
    print(f"Phase 4 — {args.timeframe} box {params['box_width_pct']}/"
          f"{params['min_bars']} across {len(symbols)} symbols.\n")

    summary_rows = []
    for i, symbol in enumerate(symbols):
        print(f"Fetching {symbol} {args.timeframe} from {args.since}...")
        df = fetch_full_history(symbol=symbol, timeframe=args.timeframe,
                                since=args.since, exchange_id=args.exchange_id)
        if df.empty:
            print(f"  no data for {symbol}, skipping")
            continue
        run_id = cr.next_run_id(args.runs_csv)
        sym = symbol.replace("/", "").lower()
        out_dir = os.path.join(args.out_root, f"phase4_{args.timeframe}",
                               f"run{run_id}_{sym}")
        res = cr.run(df, params, out_dir, symbol, args.timeframe,
                     primary_method=method, write_report=args.charts,
                     detector_cache={})
        row_args = argparse.Namespace(
            symbol=symbol, timeframe=args.timeframe, since=args.since,
            out_dir=(out_dir if args.charts else "(metrics-only)"), **params)
        cr.append_runs_csv(args.runs_csv, run_id, run_date, row_args, len(df),
                           res, only_methods=[method])
        b = {r["method"]: r for r in res["benchmark"].to_dict("records")}[method]
        edf = res["episodes_df"]
        corr = res["correlations"].get("pearson", {})
        summary_rows.append({
            "run": run_id, "symbol": symbol, "bars": len(df),
            "episodes": b["n_episodes"],
            "coverage": round(b["coverage_pct"], 3),
            "false_break": round(b["false_break_rate"], 3) if b["n_episodes"] else float("nan"),
            "dur_med_bars": round(float(edf["n_bars"].median()), 1) if not edf.empty else float("nan"),
            "width_med": round(float(edf["width_pct"].median()), 4) if not edf.empty else float("nan"),
            "esc_xatr_med": round(float(edf["escape_k_vs_atr"].median()), 2) if not edf.empty else float("nan"),
            "corr_dir": round(corr.get("breakout_direction", {}).get("width_contraction", float("nan")), 3),
        })
        print(f"  [{i+1}/{len(symbols)}] run {run_id} {symbol}: "
              f"cov={summary_rows[-1]['coverage']} fb={summary_rows[-1]['false_break']} "
              f"esc={summary_rows[-1]['esc_xatr_med']}xATR eps={b['n_episodes']}")

    import pandas as pd
    print(f"\n=== Phase 4 cross-asset ({args.timeframe}, "
          f"box {params['box_width_pct']}/{params['min_bars']}) ===")
    print(pd.DataFrame(summary_rows).to_string(index=False))
    print(f"\nLogged to {args.runs_csv}")
    return 0


def main(argv=None):
    p = argparse.ArgumentParser(description="Consolidation parameter sweep")
    p.add_argument("--phase", type=int, default=1)
    p.add_argument("--symbol", default="BTC/USDT")
    p.add_argument("--timeframe", default="1h")
    p.add_argument("--since", default="2021-01-01")
    p.add_argument("--exchange-id", default="binanceus")
    p.add_argument("--runs-csv", default="docs/research/consolidation_runs.csv")
    p.add_argument("--out-root", default="backtest/consolidation_out")
    p.add_argument("--charts", action="store_true",
                   help="render charts per cell (slow); off by default for sweeps")
    p.add_argument("--box-widths", default=None,
                   help="comma list overriding the box_width_pct axis (phase 1/3)")
    p.add_argument("--min-bars-list", default=None,
                   help="comma list overriding the min_bars axis (phase 1/3)")
    p.add_argument("--symbols",
                   default="BTC/USDT,ETH/USDT,SOL/USDT,BNB/USDT,XRP/USDT",
                   help="phase 4 only: comma list of symbols to compare")
    args = p.parse_args(argv)

    if args.phase == 4:
        return _run_phase4(args)

    method, overrides = _grid_for(args.phase, DEFAULTS)
    if args.box_widths or args.min_bars_list:
        bws = ([float(x) for x in args.box_widths.split(",")]
               if args.box_widths else [0.02])
        mbs = ([int(x) for x in args.min_bars_list.split(",")]
               if args.min_bars_list else [12])
        method = "range_containment"
        overrides = [
            {"box_width_pct": bwp, "min_bars": mb}
            for bwp, mb in itertools.product(bws, mbs)
        ]

    from data_fetcher import fetch_full_history

    print(f"Fetching {args.symbol} {args.timeframe} from {args.since} (once)...")
    df = fetch_full_history(
        symbol=args.symbol, timeframe=args.timeframe,
        since=args.since, exchange_id=args.exchange_id,
    )
    if df.empty:
        raise SystemExit("no data")
    print(f"Fetched {len(df)} bars. Sweeping {len(overrides)} cells on "
          f"'{method}' (phase {args.phase}).\n")

    run_date = datetime.date.today().isoformat()
    sym = args.symbol.replace("/", "").lower()
    summary_rows = []
    detector_cache = {}

    for i, ov in enumerate(overrides):
        params = {**DEFAULTS, **ov}
        run_id = cr.next_run_id(args.runs_csv)
        cell_tag = "_".join(f"{k}{v}" for k, v in ov.items())
        out_dir = os.path.join(
            args.out_root, f"sweep_p{args.phase}_{sym}_{args.timeframe}",
            f"run{run_id}_{cell_tag}",
        )
        res = cr.run(
            df, params, out_dir, args.symbol, args.timeframe,
            primary_method=method, write_report=args.charts,
            detector_cache=detector_cache,
        )

        row_args = argparse.Namespace(
            symbol=args.symbol, timeframe=args.timeframe, since=args.since,
            out_dir=(out_dir if args.charts else "(metrics-only)"), **params,
        )
        cr.append_runs_csv(
            args.runs_csv, run_id, run_date, row_args, len(df), res,
            only_methods=[method],
        )

        b = {r["method"]: r for r in res["benchmark"].to_dict("records")}[method]
        edf = res["episodes_df"]
        summary_rows.append({
            "run": run_id,
            **ov,
            "episodes": b["n_episodes"],
            "coverage": round(b["coverage_pct"], 3),
            "false_break": round(b["false_break_rate"], 3)
            if b["n_episodes"] else float("nan"),
            "avg_width": round(b["avg_width_pct"], 4),
            "esc_xatr_med": round(float(edf["escape_k_vs_atr"].median()), 2)
            if not edf.empty else float("nan"),
        })
        print(f"  [{i+1}/{len(overrides)}] run {run_id} {ov} -> "
              f"cov={summary_rows[-1]['coverage']} "
              f"fb={summary_rows[-1]['false_break']} "
              f"eps={b['n_episodes']}")

    import pandas as pd
    sumdf = pd.DataFrame(summary_rows)
    print(f"\n=== Phase {args.phase} ranked by coverage (target ~0.15-0.35) ===")
    sumdf["cov_dist"] = (sumdf["coverage"] - 0.25).abs()
    print(sumdf.sort_values("cov_dist").drop(columns="cov_dist").to_string(index=False))
    print(f"\nAll {len(overrides)} cells logged to {args.runs_csv}")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())

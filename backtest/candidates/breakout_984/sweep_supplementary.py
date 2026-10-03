#!/usr/bin/env python3

import argparse
import json
import os
import statistics
import sys

_HERE = os.path.dirname(os.path.abspath(__file__))
sys.path.insert(0, os.path.join(_HERE, "..", ".."))
sys.path.insert(0, os.path.join(_HERE, "..", "..", "..", "shared_tools"))
sys.path.insert(0, os.path.join(_HERE, "..", "..", "..",
                                "shared_strategies", "close"))

from eval_windows import DATASETS, WINDOWS, run_leg

STRATEGY = "breakout"

L_DEF = [{"atr_multiple": 1.5, "close_fraction": 0.4},
         {"atr_multiple": 3.0, "close_fraction": 0.8},
         {"atr_multiple": 5.0, "close_fraction": 1.0}]
L_TIGHT = [{"atr_multiple": 1.0, "close_fraction": 0.5},
           {"atr_multiple": 2.0, "close_fraction": 0.8},
           {"atr_multiple": 3.0, "close_fraction": 1.0}]
L_RUN = [{"atr_multiple": 2.0, "close_fraction": 0.33},
         {"atr_multiple": 4.0, "close_fraction": 0.66},
         {"atr_multiple": 6.0, "close_fraction": 1.0}]

R_DEF = [{"atr_multiple": 2.0, "trailing_mult_after": 1.5, "close_fraction": 0.0},
         {"atr_multiple": 2.5, "trailing_mult_after": 1.0, "close_fraction": 0.0},
         {"atr_multiple": 3.0, "trailing_mult_after": 0.8, "close_fraction": 0.0}]
R_WIDE = [{"atr_multiple": 3.0, "trailing_mult_after": 1.5, "close_fraction": 0.0},
          {"atr_multiple": 4.5, "trailing_mult_after": 1.0, "close_fraction": 0.0},
          {"atr_multiple": 6.0, "trailing_mult_after": 0.8, "close_fraction": 0.0}]


def _stack(close=None, sl=None, trail=None):
    return {"close_strategies": close, "stop_loss_atr_mult": sl,
            "trailing_stop_atr_mult": trail}


SCREENS = {
    "sweep2": {
        "tp_tight": _stack(close=[{"name": "tiered_tp_atr",
                                   "params": {"tp_tiers": L_TIGHT}}]),
        "tp_runner": _stack(close=[{"name": "tiered_tp_atr",
                                    "params": {"tp_tiers": L_RUN}}]),
        "time_stop_100": _stack(close=[{"name": "time_stop",
                                        "params": {"max_bars": 100}}]),
        "time_stop_150": _stack(close=[{"name": "time_stop",
                                        "params": {"max_bars": 150}}]),
        "time_stop_200": _stack(close=[{"name": "time_stop",
                                        "params": {"max_bars": 200}}]),
        "trail_atr_4": _stack(trail=4.0),
        "trail_atr_5": _stack(trail=5.0),
        "tp_default_trail4": _stack(close=[{"name": "tiered_tp_atr",
                                            "params": {"tp_tiers": L_DEF}}],
                                    trail=4.0),
        "tp_default_sl4": _stack(close=[{"name": "tiered_tp_atr",
                                         "params": {"tp_tiers": L_DEF}}],
                                 sl=4.0),
    },
    "timestop": {
        f"time_stop_{mb}": _stack(close=[{"name": "time_stop",
                                          "params": {"max_bars": mb}}])
        for mb in (75, 125, 175, 200, 225, 250, 300, 400)
    },
    "ratchet": {
        "ratchet_def_t3": _stack(close=[{"name": "trailing_tp_ratchet",
                                         "params": {"tp_tiers": R_DEF}}],
                                 trail=3.0),
        "ratchet_def_t4": _stack(close=[{"name": "trailing_tp_ratchet",
                                         "params": {"tp_tiers": R_DEF}}],
                                 trail=4.0),
        "ratchet_def_t5": _stack(close=[{"name": "trailing_tp_ratchet",
                                         "params": {"tp_tiers": R_DEF}}],
                                 trail=5.0),
        "ratchet_wide_t4": _stack(close=[{"name": "trailing_tp_ratchet",
                                          "params": {"tp_tiers": R_WIDE}}],
                                  trail=4.0),
        "ratchet_wide_t5": _stack(close=[{"name": "trailing_tp_ratchet",
                                          "params": {"tp_tiers": R_WIDE}}],
                                  trail=5.0),
    },
    "atrstop": {
        f"atr_stop_{mult}_{src}": _stack(
            close=[{"name": "atr_stop",
                    "params": {"atr_mult": mult, "atr_source": src}}])
        for mult in (2.0, 2.5, 3.0, 4.0, 5.0)
        for src in ("entry", "live")
    },
    "zscore": {
        f"zscore_{z}_lb{lb}": _stack(
            close=[{"name": "zscore_target",
                    "params": {"lookback": lb, "z_target": z}}])
        for z in (1.5, 2.0, 2.5, 3.0)
        for lb in (20, 50)
    },
}


def main(argv=None):
    p = argparse.ArgumentParser()
    p.add_argument("--screen", required=True, choices=sorted(SCREENS))
    p.add_argument("--window", default="is", choices=list(WINDOWS))
    p.add_argument("--json", default=None, dest="json_out")
    args = p.parse_args(argv)

    from registry_loader import load_registry
    reg = load_registry("futures")

    window = WINDOWS[args.window]
    stacks = SCREENS[args.screen]
    print(f"screen {args.screen}: {len(stacks)} stacks on window "
          f"{args.window} {window}, entry frozen at registry defaults")

    rows = []
    for label, st in stacks.items():
        legs = [run_leg(reg, STRATEGY, None, symbol, tf, window,
                        close_strategies=st["close_strategies"],
                        direction="long",
                        stop_loss_atr_mult=st["stop_loss_atr_mult"],
                        trailing_stop_atr_mult=st["trailing_stop_atr_mult"])
                for symbol, tf in DATASETS]
        rows.append({
            "label": label,
            "stack": st,
            "mean_ddadj": round(statistics.mean(l["ddadj"] for l in legs), 3),
            "mean_sharpe": round(statistics.mean(l["sharpe"] for l in legs), 3),
            "mean_ret": round(statistics.mean(l["return_pct"] for l in legs), 2),
            "worst_dd": round(min(l["max_dd_pct"] for l in legs), 2),
            "trades": sum(l["trades"] for l in legs),
        })
        r = rows[-1]
        print(f"{label:<20} DDadj {r['mean_ddadj']:>7.3f}  "
              f"Sharpe {r['mean_sharpe']:>7.2f}  ret {r['mean_ret']:>8.2f}%  "
              f"worstDD {r['worst_dd']:>9.2f}%  #T {r['trades']:>5}")

    rows.sort(key=lambda r: r["mean_ddadj"], reverse=True)
    print(f"\n== ranked by mean DDadj (window {args.window}) ==")
    for r in rows:
        print(f"{r['label']:<20} DDadj {r['mean_ddadj']:>7.3f}  "
              f"Sharpe {r['mean_sharpe']:>7.2f}  ret {r['mean_ret']:>8.2f}%  "
              f"worstDD {r['worst_dd']:>9.2f}%  #T {r['trades']:>5}")

    if args.json_out:
        with open(args.json_out, "w") as fh:
            json.dump(rows, fh, indent=2, default=str)
        print(f"\nwrote {args.json_out}")
    return 0


if __name__ == "__main__":
    sys.exit(main())

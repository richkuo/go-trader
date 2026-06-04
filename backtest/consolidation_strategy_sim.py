"""
Consolidation Range strategy simulator (research-grade, look-ahead safe).

Runs the consolidation_range entry/exit rules over historical candles WITHOUT
wiring the full open/close strategy into the scheduler, so we can measure the
edge (and the drift filter's effect) before committing to the Go/Python build.

Logic (matches the strategy spec, config A geometry close):
  - Box = trailing `min_bars` window whose high-low span is within `box_width_pct`
    of mid (computed from PAST bars only).
  - Entry near an edge: long if close is in the bottom `edge_entry_frac` of the
    box, short if in the top. Fill at NEXT bar open (no look-ahead).
  - Drift veto: net edge drift (top+bottom linear-fit travel over the window, as a
    fraction of box height) > drift_threshold suppresses shorts; < -threshold
    suppresses longs. Toggle with --no-drift-filter.
  - Exit: TP1 = box mean (close 50%), TP2 = opposite edge (close rest),
    SL = stop_atr_mult x ATR beyond the entry-side edge. Same-bar SL-before-TP
    (pessimistic). Time-stop at max_hold bars -> market exit.
  - PnL measured in R (risk = entry-to-SL distance); 1R risked per trade.

Usage:
  uv run --no-sync python backtest/consolidation_strategy_sim.py \
      --symbol BTC/USDT --timeframe 1h --since 2021-01-01 \
      --box-width-pct 0.02 --min-bars 12
"""

import argparse
import os
import sys

import numpy as np
import pandas as pd

sys.path.insert(0, os.path.dirname(__file__))
sys.path.insert(0, os.path.join(os.path.dirname(__file__), "..", "shared_tools"))

from consolidation_research import atr, _linfit_slope  # noqa: E402


def _trailing_box(highs, lows, i, win):
    seg_h = highs[i - win + 1 : i + 1]
    seg_l = lows[i - win + 1 : i + 1]
    top = seg_h.max()
    bottom = seg_l.min()
    return top, bottom, seg_h, seg_l


def simulate(df, params, regime=None):
    win = params["min_bars"]
    bwp = params["box_width_pct"]
    edge = params["edge_entry_frac"]
    stop_mult = params["stop_atr_mult"]
    drift_filter = params["drift_filter"]
    drift_thr = params["drift_threshold"]
    regime_filter = params.get("regime_filter", False)
    max_hold = params.get("max_hold", win * 3)

    highs = df["high"].to_numpy(float)
    lows = df["low"].to_numpy(float)
    opens = df["open"].to_numpy(float)
    closes = df["close"].to_numpy(float)
    atr_arr = atr(df, params.get("atr_period", 14)).to_numpy(float)
    n = len(df)

    trades = []
    i = win
    while i < n - 1:
        top, bottom, seg_h, seg_l = _trailing_box(highs, lows, i, win)
        mid = (top + bottom) / 2.0
        height = top - bottom
        if height <= 0 or mid <= 0 or (height / mid) > bwp:
            i += 1
            continue
        if regime_filter and regime is not None and regime[i] != "ranging":
            i += 1
            continue  # only open in a ranging regime (allowed_regimes=["ranging"])

        pos = (closes[i] - bottom) / height  # 0=bottom, 1=top
        # net drift over the window as a fraction of box height
        drift = (_linfit_slope(seg_h) + _linfit_slope(seg_l)) * win / height

        direction = 0
        if pos <= edge:
            direction = 1  # long at bottom
        elif pos >= 1 - edge:
            direction = -1  # short at top

        if direction == 0:
            i += 1
            continue
        if drift_filter:
            if drift > drift_thr and direction == -1:
                i += 1
                continue  # don't short an up-drifting box
            if drift < -drift_thr and direction == 1:
                i += 1
                continue  # don't long a down-drifting box

        # Enter at next bar open.
        entry = opens[i + 1]
        risk = stop_mult * atr_arr[i]
        if risk <= 0:
            i += 1
            continue
        if direction == 1:
            sl, tp1, tp2 = bottom - risk, mid, top
        else:
            sl, tp1, tp2 = top + risk, mid, bottom

        # hybrid: after TP1 at mean, trail the runner instead of capping at tp2.
        hybrid = params.get("exit_mode", "geometry") == "hybrid"
        trail_unit = params.get("trail_atr_mult", 1.5) * atr_arr[i]
        f = params.get("tp1_frac", 0.5)  # fraction scaled out at the mean (0 = full trail)
        filled_tp1 = (f <= 0.0)  # if no TP1 leg, treat as already past it
        realized_r = 0.0
        exit_idx = None
        hwm = entry  # high-water mark for the trailing runner (long); low-water for short
        run_frac = (1.0 - f)  # remaining size after the TP1 scale-out
        for j in range(i + 1, min(i + 1 + max_hold, n)):
            hi, lo = highs[j], lows[j]
            if direction == 1:
                run_stop = (hwm - trail_unit) if (filled_tp1 and hybrid) else sl
                if lo <= run_stop:  # SL / trailing stop first (pessimistic)
                    realized_r += (run_frac if filled_tp1 else 1.0) * (run_stop - entry) / risk
                    exit_idx = j
                    break
                if not filled_tp1 and hi >= tp1:
                    realized_r += f * (tp1 - entry) / risk
                    filled_tp1 = True
                    hwm = max(hwm, hi)
                if filled_tp1:
                    hwm = max(hwm, hi)
                    if hybrid:
                        continue  # let the runner trail; no fixed tp2 cap
                if hi >= tp2:  # geometry mode: cap runner at opposite edge
                    realized_r += (run_frac if filled_tp1 else 1.0) * (tp2 - entry) / risk
                    exit_idx = j
                    break
            else:
                run_stop = (hwm + trail_unit) if (filled_tp1 and hybrid) else sl
                if hi >= run_stop:
                    realized_r += (run_frac if filled_tp1 else 1.0) * (entry - run_stop) / risk
                    exit_idx = j
                    break
                if not filled_tp1 and lo <= tp1:
                    realized_r += f * (entry - tp1) / risk
                    filled_tp1 = True
                    hwm = min(hwm, lo)
                if filled_tp1:
                    hwm = min(hwm, lo)
                    if hybrid:
                        continue
                if lo <= tp2:
                    realized_r += (run_frac if filled_tp1 else 1.0) * (entry - tp2) / risk
                    exit_idx = j
                    break
        if exit_idx is None:  # time-stop: market exit at last bar's close
            exit_idx = min(i + max_hold, n - 1)
            last = closes[exit_idx]
            frac = run_frac if filled_tp1 else 1.0
            realized_r += frac * ((last - entry) if direction == 1 else (entry - last)) / risk

        # Trading cost: ~2 sides of turnover (1 in, 1 out across scale-outs) at
        # cost_bps per side, expressed in R via risk distance.
        cost_bps = params.get("cost_bps", 0.0)
        cost_r = (2.0 * (cost_bps / 10000.0) * entry / risk) if cost_bps else 0.0
        realized_r -= cost_r

        trades.append({
            "entry_idx": i + 1, "exit_idx": exit_idx, "direction": direction,
            "r": realized_r, "hit_tp1": filled_tp1,
            "bars_held": exit_idx - (i + 1),
        })
        i = exit_idx + 1  # no overlapping trades

    return pd.DataFrame(trades)


def stats(trades):
    if trades.empty:
        return {"trades": 0}
    r = trades["r"]
    wins = r[r > 0]
    losses = r[r < 0]
    equity = r.cumsum()
    peak = equity.cummax()
    max_dd = float((equity - peak).min())
    return {
        "trades": int(len(r)),
        "win_rate": round(float((r > 0).mean()), 3),
        "expectancy_R": round(float(r.mean()), 3),
        "total_R": round(float(r.sum()), 1),
        "avg_win_R": round(float(wins.mean()), 2) if len(wins) else 0.0,
        "avg_loss_R": round(float(losses.mean()), 2) if len(losses) else 0.0,
        "profit_factor": round(float(wins.sum() / -losses.sum()), 2) if len(losses) and losses.sum() != 0 else float("inf"),
        "max_dd_R": round(max_dd, 1),
        "tp1_rate": round(float(trades["hit_tp1"].mean()), 3),
        "avg_bars_held": round(float(trades["bars_held"].mean()), 1),
    }


def main(argv=None):
    p = argparse.ArgumentParser(description="Consolidation Range strategy simulator")
    p.add_argument("--symbol", default="BTC/USDT")
    p.add_argument("--timeframe", default="1h")
    p.add_argument("--since", default="2021-01-01")
    p.add_argument("--exchange-id", default="binanceus")
    p.add_argument("--box-width-pct", type=float, default=0.02)
    p.add_argument("--min-bars", type=int, default=12)
    p.add_argument("--edge-entry-frac", type=float, default=0.20)
    p.add_argument("--stop-atr-mult", type=float, default=1.0)
    p.add_argument("--drift-threshold", type=float, default=0.5)
    p.add_argument("--atr-period", type=int, default=14)
    p.add_argument("--adx-threshold", type=float, default=20.0)
    p.add_argument("--trail-atr-mult", type=float, default=1.5)
    p.add_argument("--cost-bps", type=float, default=5.0,
                   help="per-side cost (fee+slippage) in basis points; 0 disables")
    p.add_argument("--max-hold", type=int, default=None)
    args = p.parse_args(argv)

    from data_fetcher import fetch_full_history
    from regime import compute_regime
    print(f"Fetching {args.symbol} {args.timeframe} from {args.since}...")
    df = fetch_full_history(symbol=args.symbol, timeframe=args.timeframe,
                            since=args.since, exchange_id=args.exchange_id)
    if df.empty:
        raise SystemExit("no data")
    regime = compute_regime(df, period=args.atr_period,
                            adx_threshold=args.adx_threshold)["regime"].to_numpy()
    ranging_frac = float((regime == "ranging").mean())
    print(f"{len(df)} bars; ranging {ranging_frac:.0%} of the time "
          f"(ADX<{args.adx_threshold}).\n")

    base = {
        "min_bars": args.min_bars, "box_width_pct": args.box_width_pct,
        "edge_entry_frac": args.edge_entry_frac, "stop_atr_mult": args.stop_atr_mult,
        "drift_threshold": args.drift_threshold, "atr_period": args.atr_period,
        "max_hold": args.max_hold or args.min_bars * 3,
    }
    rows = []
    # (label, drift_filter, regime_filter, exit_mode)
    matrix = [
        ("geometry: baseline", False, False, "geometry"),
        ("geometry: drift only", True, False, "geometry"),
        ("geometry: regime only", False, True, "geometry"),
        ("geometry: regime+drift", True, True, "geometry"),
        ("hybrid:   baseline", False, False, "hybrid"),
        ("hybrid:   drift only", True, False, "hybrid"),
        ("hybrid:   regime+drift", True, True, "hybrid"),
    ]
    full_trades = None
    for label, drift, reg, mode in matrix:
        t = simulate(df, {**base, "drift_filter": drift, "regime_filter": reg,
                          "exit_mode": mode, "trail_atr_mult": args.trail_atr_mult,
                          "cost_bps": args.cost_bps},
                     regime)
        s = stats(t)
        s["variant"] = label
        rows.append(s)
        if mode == "hybrid" and drift and reg:
            full_trades = t
    if full_trades is not None and not full_trades.empty:
        for d, dl in [(1, "longs"), (-1, "shorts")]:
            sd = stats(full_trades[full_trades["direction"] == d])
            sd["variant"] = f"  {dl} (hybrid full)"
            rows.append(sd)

    cols = ["variant", "trades", "win_rate", "expectancy_R", "total_R",
            "profit_factor", "avg_win_R", "avg_loss_R", "max_dd_R",
            "tp1_rate", "avg_bars_held"]
    out = pd.DataFrame(rows)
    out = out[[c for c in cols if c in out.columns]]
    print(f"=== {args.symbol} {args.timeframe} box {args.box_width_pct}/"
          f"{args.min_bars} stop {args.stop_atr_mult}xATR ===")
    print(out.to_string(index=False))
    return 0


if __name__ == "__main__":
    raise SystemExit(main())

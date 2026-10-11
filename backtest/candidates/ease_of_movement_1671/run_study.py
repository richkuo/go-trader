#!/usr/bin/env python3
import argparse
import gzip
import hashlib
import io
import json
import math
import os
import sqlite3
import statistics
import subprocess
import sys
import tempfile
from datetime import datetime, timezone

HERE = os.path.dirname(os.path.abspath(__file__))
REPO = os.path.abspath(os.path.join(HERE, "..", "..", ".."))
DATA_DIR = os.path.join(HERE, "data")
MANIFEST_PATH = os.path.join(HERE, "study_manifest.json")
RESULTS_PATH = os.path.join(HERE, "results.json")
REPORT_PATH = os.path.join(HERE, "REPORT.md")
SEED_PATH = os.path.join(HERE, "candidate_seed.json")
CACHE_ENV = "GO_TRADER_OHLCV_CACHE_DB"

STUDY = "ease_of_movement_1671"
CANDIDATE = "ease_of_movement_breakout"
EXCHANGE = "binanceus"
FUNDING_EXCHANGE = "hyperliquid"
DATASETS = [
    ("BTC/USDT", "1h"), ("BTC/USDT", "4h"),
    ("ETH/USDT", "1h"), ("ETH/USDT", "4h"),
    ("SOL/USDT", "1h"), ("SOL/USDT", "4h"),
]
WINDOWS = {
    "is": ("2025-06-10", "2026-01-01"),
    "2023": ("2023-01-01", "2024-01-01"),
    "2024": ("2024-01-01", "2025-01-01"),
    "oos": ("2026-01-01", "2026-08-01"),
    "2025H1": ("2025-01-01", "2025-07-01"),
}
TRAIN_WINDOW = "is"
SCORED_WINDOWS = ("2024", "oos")
REPORTED_WINDOWS = ("2023", "2025H1")
DATA_START = "2023-01-01"
DATA_END = "2026-08-01"
CAPITAL = 1000.0
SLIPPAGE_X1 = 0.0005
SLIPPAGE_STRESS = (0.001, 0.002)
MIN_TRAIN_POSITIONS = 20
MIN_SCORED_POSITIONS = 30
LOW_VOLUME_QUANTILE = 0.10
HOUR_MS = 3_600_000
ACQUIRE_MAX_PASSES = 8
ACQUIRE_CALL_PAUSE_S = 2.0
ACQUIRE_PASS_PAUSE_S = 45.0


def _ms(date_text):
    return int(datetime.strptime(date_text, "%Y-%m-%d").replace(tzinfo=timezone.utc).timestamp() * 1000)


def _sha256(path):
    h = hashlib.sha256()
    with open(path, "rb") as fh:
        for chunk in iter(lambda: fh.read(1 << 20), b""):
            h.update(chunk)
    return h.hexdigest()


def _coin(symbol):
    return symbol.split("/")[0]


def _candle_file(symbol, timeframe):
    return f"{_coin(symbol)}_{timeframe}_candles.csv.gz"


def _write_gz_csv(path, header, rows):
    buf = io.StringIO()
    buf.write(",".join(header) + "\n")
    for row in rows:
        buf.write(",".join(repr(v) if isinstance(v, float) else str(v) for v in row) + "\n")
    with open(path, "wb") as raw:
        with gzip.GzipFile(fileobj=raw, mode="wb", mtime=0, filename="") as gz:
            gz.write(buf.getvalue().encode("utf-8"))


def _read_gz_csv(path):
    with gzip.open(path, "rt") as fh:
        lines = fh.read().splitlines()
    header = lines[0].split(",")
    return header, [line.split(",") for line in lines[1:]]


def _seed():
    with open(SEED_PATH) as fh:
        return json.load(fh)


def _git(*args):
    out = subprocess.run(["git", "-C", REPO, *args], capture_output=True, text=True)
    return out.stdout.strip()


def _import_harness():
    for path in (os.path.join(REPO, "backtest"), os.path.join(REPO, "shared_tools")):
        if path not in sys.path:
            sys.path.insert(0, path)
    import data_fetcher
    import eval_windows
    import funding_fetcher
    from optimizer import DEFAULT_PARAM_RANGES
    from registry_loader import load_registry
    return data_fetcher, eval_windows, funding_fetcher, DEFAULT_PARAM_RANGES, load_registry


def freeze(source_db):
    source_db = os.path.abspath(source_db)
    os.environ[CACHE_ENV] = source_db
    data_fetcher, _, funding_fetcher, _, _ = _import_harness()
    import time
    import pandas as pd
    pending = [(s, t, w) for s, t in DATASETS for w in sorted(WINDOWS)]
    acquired = {}
    stalled = 0
    for attempt in range(1, ACQUIRE_MAX_PASSES + 1):
        still = []
        for symbol, timeframe, window in pending:
            start, end = WINDOWS[window]
            df = data_fetcher.load_cached_data(symbol, timeframe, start_date=start, end_date=end)
            df = df[df.index < pd.Timestamp(end)]
            _, block = funding_fetcher.attach_backtest_funding(
                df, _coin(symbol), timeframe, "hyperliquid", "perps", CANDIDATE, mode="charge")
            acquired[(symbol, timeframe, window)] = {
                "dataset": f"{symbol} {timeframe}", "window": window, "bars": int(len(df)),
                "funding_complete": bool(block.get("complete")), "funding_coverage": block.get("coverage"),
                "passes": attempt, "fetch_error": block.get("error") or block.get("fetch_error")}
            if not block.get("complete"):
                still.append((symbol, timeframe, window))
            time.sleep(ACQUIRE_CALL_PAUSE_S)
        stalled = stalled + 1 if len(still) == len(pending) else 0
        pending = still
        if not pending or stalled >= 2:
            break
        time.sleep(ACQUIRE_PASS_PAUSE_S)
    acquisition = [acquired[k] for k in sorted(acquired)]
    os.makedirs(DATA_DIR, exist_ok=True)
    conn = sqlite3.connect(f"file:{source_db}?mode=ro", uri=True)
    lo, hi = _ms(DATA_START), _ms(DATA_END)
    files = {}
    for symbol, timeframe in DATASETS:
        rows = conn.execute(
            "SELECT timestamp, open, high, low, close, volume FROM ohlcv WHERE exchange=? AND symbol=? "
            "AND timeframe=? AND timestamp >= ? AND timestamp < ? ORDER BY timestamp",
            (EXCHANGE, symbol, timeframe, lo, hi)).fetchall()
        name = _candle_file(symbol, timeframe)
        _write_gz_csv(os.path.join(DATA_DIR, name), ("timestamp", "open", "high", "low", "close", "volume"),
                      [(int(r[0]), float(r[1]), float(r[2]), float(r[3]), float(r[4]), float(r[5])) for r in rows])
        files[name] = {"kind": "candles", "exchange": EXCHANGE, "symbol": symbol, "timeframe": timeframe,
                       "rows": len(rows), "first_ms": rows[0][0], "last_ms": rows[-1][0]}
    for coin in sorted({_coin(s) for s, _ in DATASETS}):
        rates = conn.execute(
            "SELECT timestamp, rate FROM funding_rates WHERE exchange=? AND coin=? AND timestamp >= ? "
            "AND timestamp <= ? ORDER BY timestamp", (FUNDING_EXCHANGE, coin, lo - 2 * HOUR_MS, hi + 2 * HOUR_MS)).fetchall()
        name = f"{coin}_funding.csv.gz"
        _write_gz_csv(os.path.join(DATA_DIR, name), ("timestamp", "rate"),
                      [(int(r[0]), float(r[1])) for r in rates])
        files[name] = {"kind": "funding_rates", "exchange": FUNDING_EXCHANGE, "coin": coin, "rows": len(rates)}
        coverage = conn.execute(
            "SELECT start_ts, end_ts FROM funding_coverage WHERE exchange=? AND coin=? AND end_ts >= ? "
            "AND start_ts <= ? ORDER BY start_ts", (FUNDING_EXCHANGE, coin, lo - 2 * HOUR_MS, hi + 2 * HOUR_MS)).fetchall()
        name = f"{coin}_funding_coverage.csv.gz"
        _write_gz_csv(os.path.join(DATA_DIR, name), ("start_ts", "end_ts"), [(int(a), int(b)) for a, b in coverage])
        files[name] = {"kind": "funding_coverage", "exchange": FUNDING_EXCHANGE, "coin": coin, "rows": len(coverage)}
        has_gaps = conn.execute(
            "SELECT name FROM sqlite_master WHERE type='table' AND name='funding_venue_gaps'").fetchone()
        gaps = []
        if has_gaps:
            gaps = conn.execute(
                "SELECT timestamp, observed_through_ms, recorded_at_ms FROM funding_venue_gaps WHERE exchange=? "
                "AND coin=? AND timestamp >= ? AND timestamp <= ? ORDER BY timestamp",
                (FUNDING_EXCHANGE, coin, lo - 2 * HOUR_MS, hi + 2 * HOUR_MS)).fetchall()
        name = f"{coin}_funding_venue_gaps.csv.gz"
        _write_gz_csv(os.path.join(DATA_DIR, name), ("timestamp", "observed_through_ms", "recorded_at_ms"),
                      [(int(a), int(b), int(c)) for a, b, c in gaps])
        files[name] = {"kind": "funding_venue_gaps", "exchange": FUNDING_EXCHANGE, "coin": coin, "rows": len(gaps)}
    conn.close()
    for name in files:
        files[name]["sha256"] = _sha256(os.path.join(DATA_DIR, name))
    manifest = {
        "study": STUDY,
        "schema": "ease_of_movement_1671/frozen_cache/v1",
        "provenance": {
            "candles": "Binance US spot OHLCV from the shared OHLCV cache (proxy for Hyperliquid perpetual prices and base volume)",
            "funding": "Hyperliquid hourly funding from the shared funding cache, refilled by shared_tools/funding_fetcher.py",
            "frozen_at_utc": datetime.now(timezone.utc).strftime("%Y-%m-%dT%H:%M:%SZ"),
            "frozen_from_commit": _git("rev-parse", "HEAD"),
        },
        "range": {"start": DATA_START, "end_exclusive": DATA_END},
        "windows": {k: {"start": v[0], "end": v[1]} for k, v in sorted(WINDOWS.items())},
        "acquisition": acquisition,
        "files": {k: files[k] for k in sorted(files)},
    }
    with open(MANIFEST_PATH, "w") as fh:
        json.dump(manifest, fh, indent=1, sort_keys=True)
        fh.write("\n")
    print(json.dumps({"frozen": sorted(files), "manifest": MANIFEST_PATH}))
    return 0


def _verify_and_build_cache(db_path):
    with open(MANIFEST_PATH) as fh:
        manifest = json.load(fh)
    for name, meta in sorted(manifest["files"].items()):
        path = os.path.join(DATA_DIR, name)
        if not os.path.isfile(path):
            raise SystemExit(f"frozen input missing: {name}")
        got = _sha256(path)
        if got != meta["sha256"]:
            raise SystemExit(f"frozen input {name} sha256 {got} != manifest {meta['sha256']}")
    from storage import init_db
    init_db(db_path)
    conn = sqlite3.connect(db_path)
    for name, meta in sorted(manifest["files"].items()):
        _, rows = _read_gz_csv(os.path.join(DATA_DIR, name))
        if meta["kind"] == "candles":
            conn.executemany(
                "INSERT INTO ohlcv (exchange, symbol, timeframe, timestamp, open, high, low, close, volume) "
                "VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)",
                [(meta["exchange"], meta["symbol"], meta["timeframe"], int(r[0]), float(r[1]), float(r[2]),
                  float(r[3]), float(r[4]), float(r[5])) for r in rows])
        elif meta["kind"] == "funding_rates":
            conn.executemany("INSERT INTO funding_rates (exchange, coin, timestamp, rate) VALUES (?, ?, ?, ?)",
                             [(meta["exchange"], meta["coin"], int(r[0]), float(r[1])) for r in rows])
        elif meta["kind"] == "funding_coverage":
            conn.executemany("INSERT INTO funding_coverage (exchange, coin, start_ts, end_ts) VALUES (?, ?, ?, ?)",
                             [(meta["exchange"], meta["coin"], int(r[0]), int(r[1])) for r in rows])
        elif meta["kind"] == "funding_venue_gaps":
            conn.executemany(
                "INSERT INTO funding_venue_gaps (exchange, coin, timestamp, observed_through_ms, recorded_at_ms) "
                "VALUES (?, ?, ?, ?, ?)",
                [(meta["exchange"], meta["coin"], int(r[0]), int(r[1]), int(r[2])) for r in rows])
    conn.commit()
    conn.close()
    return manifest


def _block_network(data_fetcher, funding_fetcher):
    def _refuse(*_args, **_kwargs):
        raise RuntimeError("network access is disabled during a frozen replay")
    data_fetcher.fetch_full_history = _refuse
    data_fetcher.get_exchange = _refuse
    funding_fetcher._hl_adapter = _refuse


def _grid(ranges):
    spec = ranges[CANDIDATE]
    out = []
    for p in spec["eom_period"]:
        for b in spec["breakout_window"]:
            out.append({"eom_period": int(p), "breakout_window": int(b)})
    return out


def _label(params):
    return f"P{params['eom_period']}_B{params['breakout_window']}"


def _summ_leg(leg):
    if leg is None:
        return None
    out = {k: leg.get(k) for k in ("sharpe", "return_pct", "max_dd_pct", "ddadj", "trades", "liquidated",
                                   "span_days", "bh_return_pct")}
    out["funding_incomplete"] = bool(leg.get("funding_incomplete"))
    funding = leg.get("funding") or {}
    out["funding"] = {"mode": funding.get("mode"), "complete": funding.get("complete"),
                      "coverage": funding.get("coverage"), "sha256": funding.get("sha256"),
                      "source": funding.get("source")}
    positions = leg.get("positions") or []
    out["positions"] = len(positions)
    out["long_positions"] = sum(1 for p in positions if p["side"] == "long")
    out["short_positions"] = sum(1 for p in positions if p["side"] == "short")
    out["fees_usd"] = round(sum(p["fees"] for p in positions), 6)
    out["net_pnl_usd"] = round(sum(p["net_pnl"] for p in positions), 6)
    out["turnover"] = round(sum(p["entry_notional"] + p["exit_notional"] for p in positions) / CAPITAL, 6)
    out["close_mode"] = (leg.get("close_validation") or {}).get("mode")
    return out


def _run(ew, reg, name, params, symbol, timeframe, window, close, stop, slippage=None):
    start, end = WINDOWS[window]
    kwargs = dict(close_strategies=close, direction="both", stop_loss_atr_mult=stop,
                  keep_positions=True, funding_mode="charge", capital=CAPITAL)
    if slippage is not None:
        kwargs["slippage_pct"] = slippage
    try:
        leg = ew.run_leg(reg, name, dict(params) if params is not None else None, symbol, timeframe,
                         (start, end), **kwargs)
        return leg, None
    except Exception as exc:
        return None, f"{type(exc).__name__}: {exc}"


def _aggregate(legs):
    present = [l for l in legs if l is not None]
    if not present:
        return None
    return {
        "legs": len(legs),
        "legs_present": len(present),
        "mean_return_pct": round(statistics.mean(l["return_pct"] for l in present), 4),
        "mean_sharpe": round(statistics.mean(l["sharpe"] for l in present), 4),
        "mean_ddadj": round(statistics.mean(l["ddadj"] for l in present), 4),
        "positions": sum(l["positions"] for l in present),
        "long_positions": sum(l["long_positions"] for l in present),
        "short_positions": sum(l["short_positions"] for l in present),
        "trades": sum(l["trades"] for l in present),
        "fees_usd": round(sum(l["fees_usd"] for l in present), 4),
        "net_pnl_usd": round(sum(l["net_pnl_usd"] for l in present), 4),
        "turnover": round(sum(l["turnover"] for l in present), 4),
        "liquidated_legs": sum(1 for l in present if l["liquidated"]),
        "funding_incomplete_legs": sum(1 for l in present if l["funding_incomplete"]),
        "missing_legs": len(legs) - len(present),
    }


def _ts(text):
    return int(datetime.fromisoformat(str(text)).replace(tzinfo=timezone.utc).timestamp() * 1000)


def _exposure(position_sets):
    events = []
    by_coin = {}
    for (symbol, timeframe), positions in position_sets.items():
        step = 3_600_000 if timeframe == "1h" else 14_400_000
        for p in positions:
            start = _ts(p["entry_date"])
            end = _ts(p["exit_date"]) + step
            events.append((start, 1))
            events.append((end, -1))
            by_coin.setdefault(_coin(symbol), {}).setdefault(timeframe, []).append((start, end))
    open_now = 0
    peak = 0
    for _, delta in sorted(events, key=lambda e: (e[0], e[1])):
        open_now += delta
        peak = max(peak, open_now)
    overlap = {}
    for coin, tfs in sorted(by_coin.items()):
        hours = 0
        for a0, a1 in tfs.get("1h", []):
            for b0, b1 in tfs.get("4h", []):
                lo, hi = max(a0, b0), min(a1, b1)
                if hi > lo:
                    hours += (hi - lo) / HOUR_MS
        overlap[coin] = round(hours, 2)
    return {"max_concurrent_positions": peak, "same_coin_overlap_hours": overlap}


def _concentration(positions):
    gains = sorted((p["net_pnl"] for p in positions if p["net_pnl"] > 0), reverse=True)
    total = sum(gains)
    if total <= 0:
        return {"top5_share_of_gains": None, "positive_positions": len(gains)}
    return {"top5_share_of_gains": round(sum(gains[:5]) / total, 4), "positive_positions": len(gains)}


def _volume_diagnostics(data_fetcher, reg, params, symbol, timeframe, window, positions):
    import numpy as np
    import pandas as pd
    start, end = WINDOWS[window]
    df = data_fetcher.load_cached_data(symbol, timeframe, start_date=start, end_date=end)
    df = df[df.index < pd.Timestamp(end)]
    vol = df["volume"].to_numpy(dtype=float)
    qs = {f"p{int(q * 100):02d}": float(np.quantile(vol, q)) for q in (0.01, 0.05, 0.10, 0.25, 0.50, 0.75, 0.95)}
    res = reg.apply_strategy(CANDIDATE, df, params)
    from ease_of_movement import required_history_bars
    need = required_history_bars(params["eom_period"], params["breakout_window"])
    post = res.iloc[need - 1:]
    invalid = post[~post["eom_valid"].astype(bool)]
    threshold = float(np.quantile(vol, LOW_VOLUME_QUANTILE))
    index = {str(ts): i for i, ts in enumerate(df.index)}
    low = 0
    matched = 0
    for p in positions:
        i = index.get(str(p["entry_date"]))
        if i is None or i < 1:
            continue
        matched += 1
        if vol[i - 1] < threshold:
            low += 1
    return {
        "bars": int(len(df)),
        "volume_quantiles": qs,
        "zero_volume_bars": int((vol <= 0).sum()),
        "invalid_bars_after_warmup": int(len(invalid)),
        "invalid_reasons": {k: int(v) for k, v in sorted(invalid["eom_reason"].value_counts().items())},
        "low_volume_threshold": threshold,
        "entries_matched": matched,
        "entries_on_low_volume_signal_bar": low,
        "low_volume_entry_share": round(low / matched, 4) if matched else None,
    }


def run():
    tmp = tempfile.mkdtemp(prefix="eom1671-")
    db_path = os.path.join(tmp, "frozen_cache.db")
    os.environ[CACHE_ENV] = db_path
    for path in (os.path.join(REPO, "backtest"), os.path.join(REPO, "shared_tools")):
        if path not in sys.path:
            sys.path.insert(0, path)
    manifest = _verify_and_build_cache(db_path)
    data_fetcher, ew, funding_fetcher, ranges, load_registry = _import_harness()
    _block_network(data_fetcher, funding_fetcher)
    reg = load_registry("futures")
    seed = _seed()
    close = seed["close_strategies"]
    stop = float(seed["stop_loss_atr_mult"])
    seed_params = dict(seed["params"])
    grid = _grid(ranges)
    results = {
        "study": STUDY,
        "candidate": CANDIDATE,
        "source_commit": _git("rev-parse", "HEAD"),
        "source_sha256": {rel: _sha256(os.path.join(REPO, rel)) for rel in (
            "shared_strategies/open/ease_of_movement.py", "shared_strategies/open/registry.py",
            "backtest/eval_windows.py", "backtest/backtester.py", "shared_tools/funding_fetcher.py",
            "shared_tools/data_fetcher.py", "shared_tools/closed_bar.py",
            "backtest/candidates/ease_of_movement_1671/run_study.py",
            "backtest/candidates/ease_of_movement_1671/candidate_seed.json")},
        "frozen_inputs": {k: v["sha256"] for k, v in sorted(manifest["files"].items())},
        "settings": {
            "datasets": [f"{s} {t}" for s, t in DATASETS],
            "windows": {k: list(v) for k, v in sorted(WINDOWS.items())},
            "train_window": TRAIN_WINDOW, "scored_windows": list(SCORED_WINDOWS),
            "reported_windows": list(REPORTED_WINDOWS),
            "close_strategies": close, "stop_loss_atr_mult": stop, "direction": "both",
            "capital": CAPITAL, "fee_platform": ew.FEE_PLATFORM, "slippage_x1": SLIPPAGE_X1,
            "slippage_stress": list(SLIPPAGE_STRESS), "funding_mode": "charge",
            "grid": grid, "seed": seed_params,
            "min_train_positions": MIN_TRAIN_POSITIONS, "min_scored_positions": MIN_SCORED_POSITIONS,
        },
        "errors": [],
    }

    train = {}
    for params in grid:
        legs = {}
        for symbol, timeframe in DATASETS:
            leg, err = _run(ew, reg, CANDIDATE, params, symbol, timeframe, TRAIN_WINDOW, close, stop)
            if err:
                results["errors"].append({"arm": _label(params), "dataset": f"{symbol} {timeframe}",
                                          "window": TRAIN_WINDOW, "error": err})
            legs[f"{symbol} {timeframe}"] = _summ_leg(leg)
        agg = _aggregate(list(legs.values()))
        eligible = bool(agg and agg["legs_present"] == len(DATASETS) and agg["liquidated_legs"] == 0
                        and agg["funding_incomplete_legs"] == 0 and agg["positions"] >= MIN_TRAIN_POSITIONS)
        train[_label(params)] = {"params": params, "aggregate": agg, "eligible": eligible, "legs": legs}
    ranked = [k for k in train if train[k]["eligible"]]
    ranked.sort(key=lambda k: (-train[k]["aggregate"]["mean_sharpe"], -train[k]["aggregate"]["mean_ddadj"],
                               list(train).index(k)))
    selected_label = ranked[0] if ranked else _label(seed_params)
    selected = dict(train[selected_label]["params"]) if ranked else seed_params
    results["train"] = train
    results["selection"] = {"selected": selected_label, "params": selected, "eligible_ranked": ranked,
                            "fallback_to_seed": not ranked}
    with open(RESULTS_PATH, "w") as fh:
        json.dump(results, fh, indent=1, sort_keys=True, default=str)
        fh.write("\n")

    windows = SCORED_WINDOWS + REPORTED_WINDOWS
    arms = {}
    positions_store = {}

    def run_arm(arm, name, params, slippage=None):
        per = {}
        for window in windows:
            for symbol, timeframe in DATASETS:
                leg, err = _run(ew, reg, name, params, symbol, timeframe, window, close, stop, slippage)
                if err:
                    results["errors"].append({"arm": arm, "dataset": f"{symbol} {timeframe}",
                                              "window": window, "error": err})
                per[f"{window}|{symbol} {timeframe}"] = _summ_leg(leg)
                positions_store[(arm, window, symbol, timeframe)] = (leg or {}).get("positions") or []
        arms[arm] = {"name": name, "params": params, "slippage_pct": slippage or SLIPPAGE_X1, "legs": per}

    for params in grid:
        run_arm(f"candidate_{_label(params)}", CANDIDATE, params)
    for b, tag in ((selected["breakout_window"], "selected"), (seed_params["breakout_window"], "seed")):
        if tag == "seed" and b == selected["breakout_window"]:
            continue
        run_arm(f"donchian_breakout_B{b}", "donchian_breakout", {"entry_period": int(b)})
        run_arm(f"chaikin_money_flow_breakout_B{b}", "chaikin_money_flow_breakout", {"breakout_window": int(b)})
    run_arm("volume_weighted", "volume_weighted", None)
    for slip in SLIPPAGE_STRESS:
        run_arm(f"candidate_{selected_label}_slip{slip}", CANDIDATE, selected, slip)

    summary = {}
    for arm, info in sorted(arms.items()):
        rows = {}
        for window in windows:
            rows[window] = _aggregate([info["legs"][f"{window}|{s} {t}"] for s, t in DATASETS])
        scored = _aggregate([info["legs"][f"{w}|{s} {t}"] for w in SCORED_WINDOWS for s, t in DATASETS])
        all_positions = [p for w in SCORED_WINDOWS for s, t in DATASETS for p in positions_store[(arm, w, s, t)]]
        exposure = {w: _exposure({(s, t): positions_store[(arm, w, s, t)] for s, t in DATASETS}) for w in windows}
        summary[arm] = {"name": info["name"], "params": info["params"], "slippage_pct": info["slippage_pct"],
                        "by_window": rows, "scored": scored, "concentration_scored": _concentration(all_positions),
                        "exposure": exposure}
    results["arms"] = {arm: {k: v for k, v in info.items()} for arm, info in sorted(arms.items())}
    results["summary"] = summary

    sel_arm = f"candidate_{selected_label}"
    diags = {}
    for window in windows:
        for symbol, timeframe in DATASETS:
            diags[f"{window}|{symbol} {timeframe}"] = _volume_diagnostics(
                data_fetcher, reg, selected, symbol, timeframe, window,
                positions_store[(sel_arm, window, symbol, timeframe)])
    results["volume_diagnostics_selected"] = diags

    results["verdict"] = verdict(summary, selected_label, selected)
    with open(RESULTS_PATH, "w") as fh:
        json.dump(results, fh, indent=1, sort_keys=True, default=str)
        fh.write("\n")
    write_report(results)
    print(json.dumps({"verdict": results["verdict"]["verdict"], "selected": selected_label}))
    return 0


def verdict(summary, selected_label, selected):
    sel = summary[f"candidate_{selected_label}"]["scored"]
    stress = summary[f"candidate_{selected_label}_slip{SLIPPAGE_STRESS[0]}"]["scored"]
    b = selected["breakout_window"]
    comparators = {
        "donchian_breakout": summary[f"donchian_breakout_B{b}"]["scored"],
        "chaikin_money_flow_breakout": summary[f"chaikin_money_flow_breakout_B{b}"]["scored"],
        "volume_weighted": summary["volume_weighted"]["scored"],
    }
    checks = []
    incomplete = []
    for arm, info in sorted(summary.items()):
        agg = info.get("scored")
        if agg is None or agg["missing_legs"] or agg["funding_incomplete_legs"]:
            incomplete.append(arm)
    checks.append({"rule": 1, "name": "coverage", "incomplete_arms": incomplete, "ok": not incomplete})
    enough = bool(sel and sel["positions"] >= MIN_SCORED_POSITIONS)
    checks.append({"rule": 2, "name": "independent_positions", "positions": sel["positions"] if sel else 0,
                   "minimum": MIN_SCORED_POSITIONS, "ok": enough})
    positive = bool(sel and sel["mean_return_pct"] > 0 and stress and stress["mean_return_pct"] > 0)
    checks.append({"rule": 3, "name": "positive_net_return_x1_and_x2",
                   "x1_mean_return_pct": sel["mean_return_pct"] if sel else None,
                   "x2_mean_return_pct": stress["mean_return_pct"] if stress else None, "ok": positive})
    beats = {}
    for name, agg in comparators.items():
        beats[name] = bool(sel and agg and sel["mean_sharpe"] > agg["mean_sharpe"]
                           and sel["mean_ddadj"] > agg["mean_ddadj"])
    checks.append({"rule": 4, "name": "beats_each_comparator", "beats": beats, "ok": all(beats.values())})
    if incomplete or not enough:
        label = "inconclusive"
        reason = "coverage incomplete" if incomplete else "fewer independent positions than the frozen minimum"
    elif not positive or not all(beats.values()):
        label = "fail"
        reason = ("mean net return not above zero at x1 and x2" if not positive
                  else "does not beat every matched comparator on mean Sharpe and drawdown-adjusted return")
    else:
        label = "inconclusive"
        reason = ("all economic checks passed, but spot-proxy candles and volume, fixed slippage and the "
                  "unmodeled lot grid and venue minimum cannot support a pass")
    return {"verdict": label, "reason": reason, "checks": checks,
            "edge_source": "study_fail" if label == "fail" else "study_inconclusive"}


def _fmt(v, nd=2):
    if v is None:
        return "n/a"
    if isinstance(v, float):
        return f"{v:.{nd}f}"
    return str(v)


def write_report(results):
    v = results["verdict"]
    sel = results["selection"]
    summary = results["summary"]
    b = sel["params"]["breakout_window"]
    lines = [
        "# Ease of Movement breakout (#1671): report",
        "",
        f"Verdict: **{v['verdict'].upper()}**. {v['reason'][0].upper() + v['reason'][1:]}.",
        f"Registry label: `edge_status=\"no_edge\"`, `edge_source=\"{v['edge_source']}\"`.",
        "",
        "This report is rendered by `run_study.py run` from [`results.json`](results.json). The frozen "
        "specification is [`README.md`](README.md); the frozen inputs and their sha256 are in "
        "[`study_manifest.json`](study_manifest.json).",
        "",
        "## Provenance",
        "",
        f"- Source commit at run time: `{results['source_commit']}` (the run may precede the final commit; "
        "the per-file source sha256 values in `results.json` pin the code that ran).",
        "- Candles and base volume: Binance US spot BTC/USDT, ETH/USDT and SOL/USDT at 1h and 4h, used as "
        "proxies for Hyperliquid perpetual prices and volume.",
        "- Funding: Hyperliquid hourly funding charged through `shared_tools/funding_fetcher.py`; the replay "
        "runs with network fetches disabled, so a missing hour is recorded as incomplete.",
        "- Fees: Hyperliquid taker 0.045% per fill. Slippage: the engine default 0.05% per fill (x1), "
        "with 0.10% (x2) and 0.20% (x4) stress runs of the selected candidate.",
        "- Not modeled on this path: the venue lot grid, the $10 venue minimum, spread separately from "
        "slippage, and market impact. Fixed slippage does not prove that a low-volume bar was executable.",
        "",
        "## Selection (training window `is`)",
        "",
        f"Selected `{sel['selected']}` (eom_period={sel['params']['eom_period']}, "
        f"breakout_window={sel['params']['breakout_window']}); fallback to seed: {sel['fallback_to_seed']}.",
        "",
        "| Variant | Eligible | Mean Sharpe | Mean DD-adj | Mean return % | Positions |",
        "|---|---|---|---|---|---|",
    ]
    for label, info in results["train"].items():
        agg = info["aggregate"] or {}
        lines.append(f"| {label} | {info['eligible']} | {_fmt(agg.get('mean_sharpe'), 3)} | "
                     f"{_fmt(agg.get('mean_ddadj'), 3)} | {_fmt(agg.get('mean_return_pct'))} | "
                     f"{agg.get('positions', 'n/a')} |")
    lines += ["", "## Scored windows (`2024`, `oos`)", "",
              f"Means are over the {len(SCORED_WINDOWS) * len(DATASETS)} scored dataset-window legs. "
              "Positions are independent opening events.", "",
              "| Arm | Mean return % | Mean Sharpe | Mean DD-adj | Positions (L/S) | Fees $ | Turnover | "
              "Top-5 share of gains | Funding-incomplete legs |",
              "|---|---|---|---|---|---|---|---|---|"]
    order = ([f"candidate_{sel['selected']}"]
             + [f"candidate_{sel['selected']}_slip{s}" for s in SLIPPAGE_STRESS]
             + [f"donchian_breakout_B{b}", f"chaikin_money_flow_breakout_B{b}", "volume_weighted"])
    order += [k for k in sorted(summary) if k not in order]
    for arm in order:
        info = summary[arm]
        agg = info["scored"] or {}
        conc = info["concentration_scored"]
        lines.append(
            f"| {arm} | {_fmt(agg.get('mean_return_pct'))} | {_fmt(agg.get('mean_sharpe'), 3)} | "
            f"{_fmt(agg.get('mean_ddadj'), 3)} | {agg.get('positions', 'n/a')} "
            f"({agg.get('long_positions', 'n/a')}/{agg.get('short_positions', 'n/a')}) | "
            f"{_fmt(agg.get('fees_usd'))} | {_fmt(agg.get('turnover'))} | "
            f"{_fmt(conc.get('top5_share_of_gains'), 3)} | {agg.get('funding_incomplete_legs', 'n/a')} |")
    lines += ["", "## Per window (selected candidate and matched comparators)", "",
              "| Arm | Window | Mean return % | Mean Sharpe | Positions | Max concurrent | Funding-incomplete legs |",
              "|---|---|---|---|---|---|---|"]
    for arm in order[:1] + order[3:6]:
        for window in list(SCORED_WINDOWS) + list(REPORTED_WINDOWS):
            agg = summary[arm]["by_window"][window] or {}
            exp = summary[arm]["exposure"][window]
            lines.append(f"| {arm} | {window} | {_fmt(agg.get('mean_return_pct'))} | "
                         f"{_fmt(agg.get('mean_sharpe'), 3)} | {agg.get('positions', 'n/a')} | "
                         f"{exp['max_concurrent_positions']} | {agg.get('funding_incomplete_legs', 'n/a')} |")
    lines += ["", "`2023` starts before the Hyperliquid funding history (first record 2023-05-12), so a "
              "position held before then has unpriced hours; `2025H1` overlaps the training window by 21 days. "
              "Neither enters the verdict.", "",
              "## Verdict checks", ""]
    for check in v["checks"]:
        detail = {k: val for k, val in check.items() if k not in ("rule", "name", "ok")}
        lines.append(f"{check['rule']}. {check['name']}: {'ok' if check['ok'] else 'not met'} "
                     f"`{json.dumps(detail, sort_keys=True)}`")
    lines += ["", "## Volume and liquidity (selected candidate)", "",
              "| Window and dataset | Bars | Volume p10 | Volume p50 | Zero-volume bars | Invalid bars after warm-up | "
              "Entries | Entries on a bottom-10% volume signal bar |",
              "|---|---|---|---|---|---|---|---|"]
    for key, d in results["volume_diagnostics_selected"].items():
        lines.append(f"| {key} | {d['bars']} | {_fmt(d['volume_quantiles']['p10'], 4)} | "
                     f"{_fmt(d['volume_quantiles']['p50'], 4)} | {d['zero_volume_bars']} | "
                     f"{d['invalid_bars_after_warmup']} | {d['entries_matched']} | "
                     f"{d['entries_on_low_volume_signal_bar']} |")
    errors = results.get("errors") or []
    lines += ["", "## Errors", "", f"{len(errors)} leg error(s)." + (" See `results.json` `errors`." if errors else ""),
              "", "## Limits", "",
              "- One fixed close ladder and one stop multiplier; other exits were not studied.",
              "- Every leg starts cold at its window start, so early bars of each window cannot enter.",
              "- A positive result on this proxy path could not pass by rule; it would need a venue-data study "
              "with lot and minimum rules.", ""]
    with open(REPORT_PATH, "w") as fh:
        fh.write("\n".join(lines))


def main(argv=None):
    parser = argparse.ArgumentParser(description="Ease of Movement breakout study (#1671)")
    sub = parser.add_subparsers(dest="cmd", required=True)
    fr = sub.add_parser("freeze", help="refill funding in a source cache and freeze the study inputs")
    fr.add_argument("--source-db", required=True)
    sub.add_parser("run", help="replay the frozen inputs and write results.json and REPORT.md")
    args = parser.parse_args(argv)
    if args.cmd == "freeze":
        return freeze(args.source_db)
    return run()


if __name__ == "__main__":
    sys.exit(main())

#!/usr/bin/env python3

import argparse
import concurrent.futures
import gzip
import hashlib
import io
import json
import os
import sqlite3
import statistics
import sys
import tempfile
import time

_HERE = os.path.dirname(os.path.abspath(__file__))
_BACKTEST = os.path.abspath(os.path.join(_HERE, "..", ".."))
_REPO = os.path.abspath(os.path.join(_BACKTEST, ".."))
for _p in (_BACKTEST, os.path.join(_REPO, "shared_tools")):
    if _p not in sys.path:
        sys.path.insert(0, _p)

CACHE_ENV = "GO_TRADER_OHLCV_CACHE_DB"
DATA_DIR = os.path.join(_HERE, "data")
FROZEN_INPUTS = os.path.join(DATA_DIR, "frozen_inputs.json")
SPEC = os.path.join(_HERE, "study_spec.json")
CANDIDATE_SEED = os.path.join(_HERE, "candidate_seed.json")

CANDIDATE = "ultimate_oscillator"
CONTROL = "donchian_breakout"
ISSUE = 1664
CANDLE_EXCHANGE = "binanceus"
FUNDING_EXCHANGE = "hyperliquid"
DATASETS = (
    ("BTC/USDT", "1h"),
    ("BTC/USDT", "4h"),
    ("ETH/USDT", "1h"),
    ("ETH/USDT", "4h"),
    ("SOL/USDT", "1h"),
    ("SOL/USDT", "4h"),
)
TF_MS = {"1h": 3_600_000, "4h": 14_400_000}
WINDOWS = {
    "is": ("2025-06-10", "2026-01-01"),
    "2023H2": ("2023-07-01", "2024-01-01"),
    "2024": ("2024-01-01", "2025-01-01"),
    "2025H1": ("2025-01-01", "2025-07-01"),
    "oos": ("2026-01-01", "2026-10-01"),
}
SELECTION_WINDOW = "is"
HELD_OUT_WINDOWS = ("2023H2", "2024", "2025H1", "oos")
FROZEN_START = "2023-07-01"
FROZEN_END = "2026-10-01"
CLOSE_STRATEGIES = [{"name": "tiered_tp_pct",
                     "params": {"tp_tiers": [{"profit_pct": 0.03, "close_fraction": 1.0}]}}]
STOP_LOSS_ATR_MULT = 1.0
DIRECTION = "both"
CAPITAL = 1000.0
FUNDING_MODE = "charge"
INTRABAR = "ohlc_walk"
STRESS_SLIPPAGE_MULT = 2.0
MIN_TRAIN_POSITIONS = 30
MIN_HELD_OUT_CLUSTERS = 30
MIN_SIDE_POSITIONS = 10
STABILITY_SHARE = 2.0 / 3.0
MAX_MISSING_BAR_SHARE = 0.005
FREEZE_RETRIES = 40
FREEZE_BACKOFF_SECONDS = 20
FREEZE_PAUSE_SECONDS = 1
SOURCE_FILES = (
    "shared_strategies/open/ultimate_oscillator.py",
    "shared_strategies/open/donchian_breakout.py",
    "shared_strategies/open/registry.py",
    "shared_strategies/close/tiered_tp_pct.py",
    "shared_tools/strategy_composition.py",
    "shared_tools/funding_fetcher.py",
    "shared_tools/storage.py",
    "shared_tools/data_fetcher.py",
    "backtest/backtester.py",
    "backtest/eval_windows.py",
    "backtest/optimizer.py",
    "backtest/parity_diff.py",
    "backtest/candidates/ultimate_oscillator_1664/run_study.py",
    "backtest/candidates/ultimate_oscillator_1664/study_spec.json",
    "backtest/candidates/ultimate_oscillator_1664/candidate_seed.json",
)


def sha256_bytes(data: bytes) -> str:
    return hashlib.sha256(data).hexdigest()


def sha256_file(path: str) -> str:
    with open(path, "rb") as fh:
        return sha256_bytes(fh.read())


def _ms(day: str) -> int:
    import pandas as pd
    return int(pd.Timestamp(day, tz="UTC").value // 1_000_000)


def _slug(symbol: str, timeframe: str) -> str:
    return f"{symbol.split('/')[0]}_{timeframe}"


def _coin(symbol: str) -> str:
    return symbol.split("/")[0]


def _write_gz(path: str, text: str) -> None:
    buf = io.BytesIO()
    with gzip.GzipFile(filename="", mode="wb", fileobj=buf, mtime=0) as gz:
        gz.write(text.encode())
    with open(path, "wb") as fh:
        fh.write(buf.getvalue())


def _read_gz_lines(path: str) -> list:
    with gzip.open(path, "rt") as fh:
        return fh.read().splitlines()


def freeze(source_db: str) -> dict:
    import data_fetcher
    import funding_fetcher
    import storage

    start_ms, end_ms = _ms(FROZEN_START), _ms(FROZEN_END)
    os.makedirs(DATA_DIR, exist_ok=True)
    files = []
    funding_db = source_db + ".funding.db"
    storage.init_db(funding_db)
    conn = sqlite3.connect(source_db)
    fconn = sqlite3.connect(funding_db)
    try:
        for symbol, tf in DATASETS:
            last = conn.execute(
                "SELECT MAX(timestamp) FROM ohlcv WHERE exchange=? AND symbol=? AND timeframe=?",
                (CANDLE_EXCHANGE, symbol, tf)).fetchone()[0]
            if last is None or last < end_ms - TF_MS[tf]:
                import pandas as pd
                since = pd.Timestamp(last or start_ms, unit="ms").strftime("%Y-%m-%d")
                data_fetcher.fetch_full_history(symbol, tf, since, CANDLE_EXCHANGE, store=True)
        import pandas as pd
        chunks = list(pd.date_range(FROZEN_START, FROZEN_END, freq="30D", tz="UTC")) + [
            pd.Timestamp(FROZEN_END, tz="UTC")]
        fetch_errors = []
        for coin in sorted({_coin(s) for s, _ in DATASETS}):
            for lo, hi in zip(chunks, chunks[1:]):
                if hi <= lo:
                    continue
                for attempt in range(FREEZE_RETRIES):
                    box = {}
                    funding_fetcher.load_cached_funding(
                        coin, lo, end_date=hi, exchange=FUNDING_EXCHANGE, db_path=funding_db,
                        source_box=box)
                    time.sleep(FREEZE_PAUSE_SECONDS)
                    if not box.get("fetch_error"):
                        break
                    time.sleep(FREEZE_BACKOFF_SECONDS)
                else:
                    fetch_errors.append(f"{coin} {lo}..{hi}: {box['fetch_error'][:300]}")
        if fetch_errors:
            raise SystemExit("funding download errors: " + "; ".join(fetch_errors))
        for symbol, tf in DATASETS:
            rows = conn.execute(
                "SELECT timestamp, open, high, low, close, volume FROM ohlcv WHERE exchange=? "
                "AND symbol=? AND timeframe=? AND timestamp >= ? AND timestamp < ? ORDER BY timestamp",
                (CANDLE_EXCHANGE, symbol, tf, start_ms, end_ms)).fetchall()
            text = "timestamp,open,high,low,close,volume\n" + "".join(
                f"{r[0]},{r[1]!r},{r[2]!r},{r[3]!r},{r[4]!r},{r[5]!r}\n" for r in rows)
            name = f"{_slug(symbol, tf)}_candles.csv.gz"
            _write_gz(os.path.join(DATA_DIR, name), text)
            files.append({"kind": "candles", "dataset": f"{symbol} {tf}", "symbol": symbol,
                          "timeframe": tf, "path": name, "rows": len(rows),
                          "first_ms": rows[0][0] if rows else None,
                          "last_ms": rows[-1][0] if rows else None})
        for coin in sorted({_coin(s) for s, _ in DATASETS}):
            rates = fconn.execute(
                "SELECT timestamp, rate FROM funding_rates WHERE exchange=? AND coin=? "
                "AND timestamp >= ? AND timestamp <= ? ORDER BY timestamp",
                (FUNDING_EXCHANGE, coin, start_ms - 3_600_000, end_ms)).fetchall()
            gaps = fconn.execute(
                "SELECT timestamp, observed_through_ms, recorded_at_ms FROM funding_venue_gaps "
                "WHERE exchange=? AND coin=? AND timestamp >= ? AND timestamp <= ? ORDER BY timestamp",
                (FUNDING_EXCHANGE, coin, start_ms - 3_600_000, end_ms)).fetchall()
            coverage = [(max(s, start_ms - 3_600_000), min(e, end_ms)) for s, e in
                        storage.load_funding_coverage(FUNDING_EXCHANGE, coin, db_path=funding_db)
                        if e >= start_ms - 3_600_000 and s <= end_ms]
            rate_text = "timestamp,rate\n" + "".join(f"{t},{r!r}\n" for t, r in rates)
            rate_name = f"{coin}_funding.csv.gz"
            _write_gz(os.path.join(DATA_DIR, rate_name), rate_text)
            files.append({"kind": "funding", "coin": coin, "path": rate_name, "rows": len(rates),
                          "first_ms": rates[0][0] if rates else None,
                          "last_ms": rates[-1][0] if rates else None})
            meta_name = f"{coin}_funding_meta.json"
            with open(os.path.join(DATA_DIR, meta_name), "w") as fh:
                json.dump({"coin": coin, "coverage": coverage,
                           "venue_gaps": [list(g) for g in gaps]}, fh, indent=1, sort_keys=True)
                fh.write("\n")
            files.append({"kind": "funding_meta", "coin": coin, "path": meta_name,
                          "coverage_intervals": len(coverage), "venue_gap_hours": len(gaps)})
    finally:
        conn.close()
        fconn.close()
    for f in files:
        f["sha256"] = sha256_file(os.path.join(DATA_DIR, f["path"]))
    manifest = {
        "study": "ultimate_oscillator_1664",
        "candle_source": f"{CANDLE_EXCHANGE} spot OHLCV via ccxt (proxy for Hyperliquid perpetual prices)",
        "funding_source": "Hyperliquid public info API fundingHistory via shared_tools/funding_fetcher.py",
        "span": {"start": FROZEN_START, "end": FROZEN_END},
        "files": files,
    }
    with open(FROZEN_INPUTS, "w") as fh:
        json.dump(manifest, fh, indent=1, sort_keys=True)
        fh.write("\n")
    return manifest


def load_frozen_inputs() -> dict:
    with open(FROZEN_INPUTS) as fh:
        manifest = json.load(fh)
    for f in manifest["files"]:
        actual = sha256_file(os.path.join(DATA_DIR, f["path"]))
        if actual != f["sha256"]:
            raise SystemExit(f"frozen input {f['path']} sha256 {actual} != pinned {f['sha256']}")
    return manifest


def build_frozen_cache(manifest: dict, db_path: str) -> None:
    import storage
    storage.init_db(db_path)
    conn = sqlite3.connect(db_path)
    try:
        for f in manifest["files"]:
            path = os.path.join(DATA_DIR, f["path"])
            if f["kind"] == "candles":
                lines = _read_gz_lines(path)[1:]
                rows = []
                for line in lines:
                    t, o, h, l, c, v = line.split(",")
                    rows.append((CANDLE_EXCHANGE, f["symbol"], f["timeframe"], int(t), float(o),
                                 float(h), float(l), float(c), float(v)))
                conn.executemany(
                    "INSERT INTO ohlcv (exchange, symbol, timeframe, timestamp, open, high, low, close,"
                    " volume) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)", rows)
            elif f["kind"] == "funding":
                lines = _read_gz_lines(path)[1:]
                conn.executemany(
                    "INSERT INTO funding_rates (exchange, coin, timestamp, rate) VALUES (?, ?, ?, ?)",
                    [(FUNDING_EXCHANGE, f["coin"], int(t), float(r))
                     for t, r in (line.split(",") for line in lines)])
            elif f["kind"] == "funding_meta":
                with open(path) as fh:
                    meta = json.load(fh)
                conn.executemany(
                    "INSERT INTO funding_coverage (exchange, coin, start_ts, end_ts) VALUES (?, ?, ?, ?)",
                    [(FUNDING_EXCHANGE, f["coin"], int(s), int(e)) for s, e in meta["coverage"]])
                conn.executemany(
                    "INSERT INTO funding_venue_gaps (exchange, coin, timestamp, observed_through_ms,"
                    " recorded_at_ms) VALUES (?, ?, ?, ?, ?)",
                    [(FUNDING_EXCHANGE, f["coin"], int(t), int(o), int(r))
                     for t, o, r in meta["venue_gaps"]])
        conn.commit()
    finally:
        conn.close()


def forbid_network() -> None:
    import data_fetcher
    import funding_fetcher

    def _refuse(*_a, **_k):
        raise RuntimeError("frozen study run: network access is disabled")

    data_fetcher.fetch_full_history = _refuse
    funding_fetcher._hl_adapter = _refuse


_CAPTURED = {}


def install_result_capture() -> None:
    from backtester import Backtester
    if getattr(Backtester.run, "_uo_capture", False):
        return
    original = Backtester.run

    def run(self, *args, **kwargs):
        res = original(self, *args, **kwargs)
        _CAPTURED["results"] = res
        _CAPTURED["slippage_pct"] = self.slippage_pct
        _CAPTURED["commission_pct"] = self.commission_pct
        return res

    run._uo_capture = True
    Backtester.run = run


class MaskedControlRegistry:
    def __init__(self, reg, candidate_params: dict):
        self._reg = reg
        self._candidate_params = dict(candidate_params)
        self.STRATEGY_REGISTRY = reg.STRATEGY_REGISTRY

    def list_strategies(self):
        return self._reg.list_strategies()

    def apply_strategy(self, name, df, params=None):
        out = self._reg.apply_strategy(name, df, params)
        if name != CONTROL:
            return out
        cand = self._reg.apply_strategy(CANDIDATE, df, self._candidate_params)
        valid = cand["uo_valid"].to_numpy(dtype=bool)
        out = out.copy()
        out["control_raw_signal"] = out["signal"].to_numpy()
        out["control_mask_valid"] = valid
        out.loc[~valid, "signal"] = 0
        return out


def _registry():
    from registry_loader import load_registry
    return load_registry("futures")


def control_params(candidate_params: dict) -> dict:
    return {"entry_period": int(candidate_params["breakout_lookback"]), "exit_period": 10}


def run_arm_leg(reg, name: str, params: dict, symbol: str, timeframe: str, window_name: str,
                slippage_pct=None) -> dict:
    from eval_windows import execution_metrics, run_leg
    _CAPTURED.clear()
    leg = run_leg(reg, name, params, symbol, timeframe, WINDOWS[window_name], capital=CAPITAL,
                  close_strategies=CLOSE_STRATEGIES, direction=DIRECTION,
                  stop_loss_atr_mult=STOP_LOSS_ATR_MULT, intrabar_resolution=INTRABAR,
                  slippage_pct=slippage_pct, funding_mode=FUNDING_MODE)
    if leg is None:
        return {"dataset": f"{symbol} {timeframe}", "window": window_name, "missing": True}
    results = _CAPTURED["results"]
    ex = execution_metrics(results, CAPITAL)
    positions = ex.pop("position_list")
    funding = leg.get("funding") or {}
    return {
        "dataset": f"{symbol} {timeframe}",
        "window": window_name,
        "sharpe": leg["sharpe"],
        "return_pct": leg["return_pct"],
        "max_dd_pct": leg["max_dd_pct"],
        "ddadj": leg["ddadj"],
        "liquidated": leg["liquidated"],
        "bh_return_pct": leg["bh_return_pct"],
        "trade_records": leg["trades"],
        "commission_pct": _CAPTURED["commission_pct"],
        "slippage_pct": _CAPTURED["slippage_pct"],
        "funding_complete": bool(funding.get("complete")) and not leg.get("funding_incomplete"),
        "funding": {k: funding.get(k) for k in ("mode", "available", "complete", "coverage", "source",
                                                 "sha256", "row_count", "first", "last", "priced",
                                                 "error", "fetch_error") if k in funding},
        "close_validation": leg.get("close_validation"),
        "execution": ex,
        "positions": [{"entry": p["entry_date"], "exit": p["exit_date"], "side": p["side"],
                       "net_pnl": p["net_pnl"], "fees": p["fees"],
                       "exit_reasons": sorted(set(p["exit_reasons"]))} for p in positions],
    }


def _leg_task(args):
    kind, name, params, symbol, timeframe, window_name, slippage, mask_params = args
    reg = _registry()
    if mask_params is not None:
        reg = MaskedControlRegistry(reg, mask_params)
    return run_arm_leg(reg, name, params, symbol, timeframe, window_name, slippage)


def _pool_init(db_path: str) -> None:
    os.environ[CACHE_ENV] = db_path
    forbid_network()
    install_result_capture()


class Runner:
    def __init__(self, db_path: str, workers: int):
        self.db_path = db_path
        self.workers = workers
        self.cache = {}
        self.pool = None

    def close(self) -> None:
        if self.pool is not None:
            self.pool.shutdown()
            self.pool = None

    def legs(self, tasks: list) -> list:
        keys = [json.dumps(t, sort_keys=True, default=str) for t in tasks]
        todo = [(k, t) for k, t in zip(keys, tasks) if k not in self.cache]
        seen = set()
        uniq = []
        for k, t in todo:
            if k not in seen:
                seen.add(k)
                uniq.append((k, t))
        if uniq:
            if self.workers > 1:
                if self.pool is None:
                    self.pool = concurrent.futures.ProcessPoolExecutor(
                        max_workers=self.workers, initializer=_pool_init,
                        initargs=(self.db_path,))
                for (k, _), res in zip(uniq, self.pool.map(_leg_task, [t for _, t in uniq],
                                                            chunksize=4)):
                    self.cache[k] = res
            else:
                for k, t in uniq:
                    self.cache[k] = _leg_task(t)
        return [self.cache[k] for k in keys]


def task(name, params, window_name, slippage=None, mask_params=None, datasets=DATASETS):
    return [("arm", name, dict(params), s, tf, window_name, slippage,
             None if mask_params is None else dict(mask_params)) for s, tf in datasets]


def clusters(positions: list) -> int:
    import pandas as pd
    spans = sorted((pd.Timestamp(p["entry"]), pd.Timestamp(p["exit"]), p["side"], p["window"])
                   for p in positions)
    count = 0
    open_until = {}
    for start, end, side, window in spans:
        key = (side, window)
        if key in open_until and start <= open_until[key]:
            open_until[key] = max(open_until[key], end)
        else:
            count += 1
            open_until[key] = end
    return count


def summarize(legs: list, bars: dict) -> dict:
    present = [l for l in legs if not l.get("missing")]
    positions = [dict(p, dataset=l["dataset"], window=l["window"]) for l in present
                 for p in l["positions"]]
    per_leg = {}
    for l in present:
        longs = [p for p in l["positions"] if p["side"] == "long"]
        shorts = [p for p in l["positions"] if p["side"] == "short"]
        bar = bars.get((l["window"], l["dataset"])) if bars else None
        per_leg[f"{l['window']} | {l['dataset']}"] = {
            "return_pct": l["return_pct"], "sharpe": l["sharpe"], "ddadj": l["ddadj"],
            "max_dd_pct": l["max_dd_pct"], "net_pnl_usd": l["execution"]["net_pnl_usd"],
            "fees_usd": l["execution"]["fees_usd"],
            "funding_pnl_usd": l["execution"]["funding_pnl_usd"],
            "turnover": l["execution"]["turnover"],
            "positions": len(l["positions"]), "long_positions": len(longs),
            "short_positions": len(shorts),
            "long_net_usd": round(float(sum(p["net_pnl"] for p in longs)), 6),
            "short_net_usd": round(float(sum(p["net_pnl"] for p in shorts)), 6),
            "funding_complete": l["funding_complete"], "liquidated": l["liquidated"],
            "incumbent_bar_sharpe": bar["sharpe"] if bar else None,
            "incumbent_bar_ddadj": bar["ddadj"] if bar else None,
        }
    if not present:
        return {"legs": 0, "expected_legs": len(legs)}
    abs_leg = [abs(v["net_pnl_usd"]) for v in per_leg.values()]
    abs_pos = sorted((abs(p["net_pnl"]) for p in positions), reverse=True)
    with_bar = [l for l in present if bars and bars.get((l["window"], l["dataset"]))]
    out = {
        "legs": len(present),
        "expected_legs": len(legs),
        "mean_sharpe": round(statistics.mean(l["sharpe"] for l in present), 4),
        "mean_ddadj": round(statistics.mean(l["ddadj"] for l in present), 4),
        "mean_return_pct": round(statistics.mean(l["return_pct"] for l in present), 4),
        "worst_max_dd_pct": round(min(l["max_dd_pct"] for l in present), 4),
        "net_pnl_usd": round(sum(l["execution"]["net_pnl_usd"] for l in present), 4),
        "fees_usd": round(sum(l["execution"]["fees_usd"] for l in present), 4),
        "funding_pnl_usd": round(sum(l["execution"]["funding_pnl_usd"] for l in present), 4),
        "mean_turnover": round(statistics.mean(l["execution"]["turnover"] for l in present), 4),
        "positions": len(positions),
        "long_positions": sum(1 for p in positions if p["side"] == "long"),
        "short_positions": sum(1 for p in positions if p["side"] == "short"),
        "long_net_usd": round(sum(p["net_pnl"] for p in positions if p["side"] == "long"), 4),
        "short_net_usd": round(sum(p["net_pnl"] for p in positions if p["side"] == "short"), 4),
        "independent_clusters": clusters(positions),
        "liquidated_legs": sum(1 for l in present if l["liquidated"]),
        "funding_complete": all(l["funding_complete"] for l in present),
        "top_leg_abs_pnl_share": round(max(abs_leg) / sum(abs_leg), 4) if sum(abs_leg) > 0 else None,
        "top5_position_abs_pnl_share": (round(sum(abs_pos[:5]) / sum(abs_pos), 4)
                                        if sum(abs_pos) > 0 else None),
        "per_leg": per_leg,
    }
    if with_bar:
        out["bar_mean_sharpe"] = round(statistics.mean(bars[(l["window"], l["dataset"])]["sharpe"]
                                                       for l in with_bar), 4)
        out["bar_mean_ddadj"] = round(statistics.mean(bars[(l["window"], l["dataset"])]["ddadj"]
                                                      for l in with_bar), 4)
        out["bar_funding_complete"] = not any(
            bars[(l["window"], l["dataset"])].get("funding_incomplete") for l in with_bar)
        out["beats_bar_sharpe_legs"] = sum(
            1 for l in with_bar if l["sharpe"] > bars[(l["window"], l["dataset"])]["sharpe"])
        out["beats_bar_ddadj_legs"] = sum(
            1 for l in with_bar if l["ddadj"] > bars[(l["window"], l["dataset"])]["ddadj"])
    return out


def grid(name: str, defaults: dict) -> list:
    from eval_windows import expand_sweep
    from optimizer import DEFAULT_PARAM_RANGES
    specs = [(k, list(v)) for k, v in DEFAULT_PARAM_RANGES[name].items()]
    return expand_sweep(dict(defaults), specs)


def neighbors(params: dict, rows: list) -> list:
    from optimizer import DEFAULT_PARAM_RANGES
    ranges = DEFAULT_PARAM_RANGES[CANDIDATE]
    out = []
    for r in rows:
        diffs = [abs(ranges[k].index(params[k]) - ranges[k].index(r["params"][k])) for k in ranges]
        if sum(diffs) == 1:
            out.append(r)
    return out


def select(rows: list) -> dict:
    eligible = [(i, r) for i, r in enumerate(rows)
                if r["summary"]["legs"] == r["summary"]["expected_legs"]
                and r["summary"]["liquidated_legs"] == 0
                and r["summary"]["positions"] >= MIN_TRAIN_POSITIONS]
    if not eligible:
        return {"rule_outcome": "no_eligible_combo", "selected": None, "eligible": 0}
    best = max(eligible, key=lambda ir: (ir[1]["summary"]["mean_sharpe"],
                                         ir[1]["summary"]["mean_ddadj"], -ir[0]))[1]
    return {"rule_outcome": "selected", "selected": best["label"], "params": best["params"],
            "eligible": len(eligible)}


def candle_coverage(db_path: str) -> dict:
    import pandas as pd
    out = {}
    conn = sqlite3.connect(db_path)
    try:
        for wname, (start, end) in WINDOWS.items():
            for symbol, tf in DATASETS:
                lo, hi = _ms(start), _ms(end)
                ts = [r[0] for r in conn.execute(
                    "SELECT timestamp FROM ohlcv WHERE exchange=? AND symbol=? AND timeframe=? AND "
                    "timestamp >= ? AND timestamp < ? ORDER BY timestamp",
                    (CANDLE_EXCHANGE, symbol, tf, lo, hi))]
                expected = (hi - lo) // TF_MS[tf]
                gaps = [[str(pd.Timestamp(a, unit="ms")), str(pd.Timestamp(b, unit="ms")),
                         (b - a) // TF_MS[tf] - 1]
                        for a, b in zip(ts, ts[1:]) if b - a != TF_MS[tf]]
                missing = expected - len(ts)
                out[f"{wname} | {symbol} {tf}"] = {
                    "expected_bars": expected, "present_bars": len(ts), "missing_bars": missing,
                    "gaps": gaps,
                    "first": str(pd.Timestamp(ts[0], unit="ms")) if ts else None,
                    "last": str(pd.Timestamp(ts[-1], unit="ms")) if ts else None,
                    "complete": bool(ts) and ts[0] == lo and ts[-1] == hi - TF_MS[tf]
                    and missing <= MAX_MISSING_BAR_SHARE * expected,
                }
    finally:
        conn.close()
    return out


def mask_audit(reg, params: dict, window_names) -> dict:
    from data_fetcher import load_cached_data
    import pandas as pd
    out = {}
    cparams = control_params(params)
    for wname in window_names:
        start, end = WINDOWS[wname]
        for symbol, tf in DATASETS:
            df = load_cached_data(symbol, tf, start_date=start, end_date=end)
            df = df[df.index < pd.Timestamp(end)]
            cand = reg.apply_strategy(CANDIDATE, df, params)
            raw = reg.apply_strategy(CONTROL, df, cparams)
            masked = MaskedControlRegistry(reg, params).apply_strategy(CONTROL, df, cparams)
            valid = cand["uo_valid"].to_numpy(dtype=bool)
            rs = raw["signal"].to_numpy()
            ms_ = masked["signal"].to_numpy()
            cs = cand["signal"].to_numpy()
            reasons = cand["uo_reason"].value_counts().to_dict()
            out[f"{wname} | {symbol} {tf}"] = {
                "bars": int(len(df)),
                "valid_bars": int(valid.sum()),
                "mask_equals_validity": bool((masked["control_mask_valid"].to_numpy(dtype=bool)
                                              == valid).all()),
                "masked_entries_are_raw_events": bool(((ms_ == 0) | (ms_ == rs)).all()),
                "masked_entries_only_on_valid_bars": bool((ms_[~valid] == 0).all()),
                "candidate_entries_subset_of_masked": bool(((cs == 0) | (cs == ms_)).all()),
                "candidate_events_match_raw_donchian": bool(
                    (cand["uo_raw_event"].to_numpy()[valid] == rs[valid]).all()),
                "raw_events": int((rs != 0).sum()),
                "masked_events": int((ms_ != 0).sum()),
                "candidate_entries": int((cs != 0).sum()),
                "candidate_long": int((cs == 1).sum()),
                "candidate_short": int((cs == -1).sum()),
                "reasons": {k: int(v) for k, v in sorted(reasons.items())},
            }
    return out


def incumbent_bar_map(reg, window_names) -> tuple:
    from eval_windows import INCUMBENTS, compute_incumbent_legs, incumbent_bars
    bars, raw = {}, {}
    for wname in window_names:
        legs = compute_incumbent_legs(reg, list(DATASETS), WINDOWS[wname], CAPITAL,
                                      intrabar_resolution=INTRABAR, funding_mode=FUNDING_MODE)
        b = incumbent_bars(legs)
        for ds, bar in b.items():
            bars[(wname, ds)] = bar
        raw[wname] = {ds: {n: (None if leg is None else
                               {"sharpe": leg["sharpe"], "ddadj": leg["ddadj"],
                                "return_pct": leg["return_pct"], "trades": leg["trades"],
                                "funding_incomplete": bool(leg.get("funding_incomplete"))})
                           for n, leg in sorted(per.items())} for ds, per in sorted(legs.items())}
    return bars, raw, list(INCUMBENTS)


def verdict(cand: dict, stress: dict, control: dict, neighbor_rows: list, coverage: dict,
            control_cov_ok: bool) -> dict:
    held_cov = [v for k, v in coverage.items() if k.split(" | ")[0] in HELD_OUT_WINDOWS]
    checks = {
        "candle_coverage_complete": all(v["complete"] for v in held_cov),
        "funding_coverage_complete": (cand["funding_complete"] and stress["funding_complete"]
                                      and control_cov_ok and cand.get("bar_funding_complete", False)),
        "all_legs_scored": cand["legs"] == cand["expected_legs"],
        "sample_clusters": cand["independent_clusters"] >= MIN_HELD_OUT_CLUSTERS,
        "sample_long": cand["long_positions"] >= MIN_SIDE_POSITIONS,
        "sample_short": cand["short_positions"] >= MIN_SIDE_POSITIONS,
        "no_liquidation": cand["liquidated_legs"] == 0 and stress["liquidated_legs"] == 0,
        "net_positive_base": cand["net_pnl_usd"] > 0,
        "net_positive_stress": stress["net_pnl_usd"] > 0,
        "beats_incumbent_bar_sharpe": cand["mean_sharpe"] > cand.get("bar_mean_sharpe", float("inf")),
        "beats_incumbent_bar_ddadj": cand["mean_ddadj"] > cand.get("bar_mean_ddadj", float("inf")),
        "beats_paired_control_sharpe": cand["mean_sharpe"] > control["mean_sharpe"],
        "beats_paired_control_ddadj": cand["mean_ddadj"] > control["mean_ddadj"],
    }
    evaluable = [r for r in neighbor_rows if r.get("evaluable")]
    stable = [r for r in evaluable if r["net_positive"] and r["beats_control_sharpe"]
              and r["beats_control_ddadj"]]
    checks["neighbors_evaluable"] = bool(evaluable)
    checks["neighbor_stability"] = bool(evaluable) and len(stable) >= STABILITY_SHARE * len(evaluable)
    coverage_keys = ("candle_coverage_complete", "funding_coverage_complete", "all_legs_scored")
    sample_keys = ("sample_clusters", "sample_long", "sample_short")
    if not all(checks[k] for k in coverage_keys):
        failed = sorted(k for k in coverage_keys if not checks[k])
        outcome, reason = "inconclusive", "held-out coverage incomplete: " + ", ".join(failed)
    elif not all(checks[k] for k in sample_keys):
        failed = sorted(k for k in sample_keys if not checks[k])
        outcome, reason = "inconclusive", "held-out sample below the frozen minimum: " + ", ".join(failed)
    elif not checks["neighbors_evaluable"]:
        outcome, reason = "inconclusive", "no evaluable neighbouring variant for the stability rule"
    elif all(checks.values()):
        outcome, reason = "pass", "every frozen held-out check passed"
    else:
        failed = sorted(k for k, v in checks.items() if not v)
        outcome, reason = "fail", "failed frozen checks: " + ", ".join(failed)
    return {"outcome": outcome, "reason": reason, "checks": checks,
            "stable_neighbors": len(stable), "evaluable_neighbors": len(evaluable),
            "live_status": "research-only; edge_status no_edge regardless of outcome"}


def run(runner: Runner, frozen: dict) -> dict:
    db_path = runner.db_path
    reg = _registry()
    from backtester import DEFAULT_SLIPPAGE_PCT, aggregate_close_validations, fee_pct_for_platform
    with open(SPEC) as fh:
        spec = json.load(fh)
    stress_slippage = DEFAULT_SLIPPAGE_PCT * STRESS_SLIPPAGE_MULT

    defaults = dict(reg.STRATEGY_REGISTRY[CANDIDATE]["default_params"])
    with open(CANDIDATE_SEED) as fh:
        seed = json.load(fh)["params"]
    if seed != defaults:
        raise SystemExit(f"candidate_seed.json params {seed} != registry defaults {defaults}")

    bars, incumbent_raw, incumbents = incumbent_bar_map(
        reg, (SELECTION_WINDOW,) + HELD_OUT_WINDOWS)

    combos = grid(CANDIDATE, defaults)
    train_tasks = [task(CANDIDATE, p, SELECTION_WINDOW) for _, p in combos]
    flat = runner.legs([t for ts in train_tasks for t in ts])
    train_rows = []
    for i, (label, params) in enumerate(combos):
        legs = flat[i * len(DATASETS):(i + 1) * len(DATASETS)]
        s = summarize(legs, bars)
        s.pop("per_leg")
        train_rows.append({"label": label, "params": params, "summary": s})
    choice = select(train_rows)
    selected = choice["params"] if choice["selected"] else defaults

    def held(name, params, slippage=None, mask=None):
        legs = runner.legs([t for w in HELD_OUT_WINDOWS
                            for t in task(name, params, w, slippage, mask)])
        return summarize(legs, bars), legs

    cand_base, cand_legs = held(CANDIDATE, selected)
    cand_stress, _ = held(CANDIDATE, selected, stress_slippage)
    seed_base, _ = held(CANDIDATE, defaults)
    control_base, control_legs = held(CONTROL, control_params(selected), mask=selected)
    control_stress, _ = held(CONTROL, control_params(selected), stress_slippage, mask=selected)
    control_raw, _ = held(CONTROL, control_params(selected))
    control_cov_ok = control_base["funding_complete"] and control_stress["funding_complete"]

    neighbor_rows = []
    for r in neighbors(selected, train_rows):
        nb, _ = held(CANDIDATE, r["params"])
        nc, _ = held(CONTROL, control_params(r["params"]), mask=r["params"])
        evaluable = (nb["legs"] == nb["expected_legs"] and nb["funding_complete"]
                     and nc["funding_complete"])
        neighbor_rows.append({
            "label": r["label"], "params": r["params"],
            "train_mean_sharpe": r["summary"]["mean_sharpe"],
            "held_mean_sharpe": nb["mean_sharpe"], "held_mean_ddadj": nb["mean_ddadj"],
            "held_net_pnl_usd": nb["net_pnl_usd"], "held_positions": nb["positions"],
            "control_mean_sharpe": nc["mean_sharpe"], "control_mean_ddadj": nc["mean_ddadj"],
            "control_net_pnl_usd": nc["net_pnl_usd"],
            "evaluable": evaluable,
            "net_positive": nb["net_pnl_usd"] > 0,
            "beats_control_sharpe": nb["mean_sharpe"] > nc["mean_sharpe"],
            "beats_control_ddadj": nb["mean_ddadj"] > nc["mean_ddadj"],
        })

    coverage = candle_coverage(db_path)
    v = verdict(cand_base, cand_stress, control_base, neighbor_rows, coverage, control_cov_ok)
    per_window = {}
    for w in HELD_OUT_WINDOWS:
        cw = summarize([l for l in cand_legs if l["window"] == w], bars)
        kw = summarize([l for l in control_legs if l["window"] == w], bars)
        cw.pop("per_leg")
        kw.pop("per_leg")
        per_window[w] = {"candidate": cw, "control_masked": kw}

    return {
        "study": "ultimate_oscillator_1664",
        "issue": ISSUE,
        "spec_sha256": sha256_file(SPEC),
        "frozen_inputs": {"path": os.path.relpath(FROZEN_INPUTS, _REPO),
                          "sha256": sha256_file(FROZEN_INPUTS),
                          "files": frozen["files"],
                          "candle_source": frozen["candle_source"],
                          "funding_source": frozen["funding_source"]},
        "source_sha256": {p: sha256_file(os.path.join(_REPO, p)) for p in SOURCE_FILES},
        "protocol": {
            "datasets": [f"{s} {tf}" for s, tf in DATASETS],
            "windows": {k: list(v) for k, v in WINDOWS.items()},
            "selection_window": SELECTION_WINDOW,
            "held_out_windows": list(HELD_OUT_WINDOWS),
            "close_strategies": CLOSE_STRATEGIES,
            "stop_loss_atr_mult": STOP_LOSS_ATR_MULT,
            "direction": DIRECTION,
            "capital": CAPITAL,
            "funding_mode": FUNDING_MODE,
            "intrabar_resolution": INTRABAR,
            "comparison_mode": "strict",
            "taker_fee_pct": fee_pct_for_platform("hyperliquid"),
            "base_slippage_pct": DEFAULT_SLIPPAGE_PCT,
            "stress_slippage_pct": stress_slippage,
            "incumbents": incumbents,
            "control": {"name": CONTROL, "params_rule": "entry_period = breakout_lookback, exit_period 10 "
                        "(unused: the close strategy and stop own every exit)",
                        "mask": "entries zeroed where the candidate uo_valid is false"},
            "spec": spec,
        },
        "train": {"grid_size": len(combos), "rows": train_rows, "selection": choice},
        "selected_params": selected,
        "held_out": {
            "candidate_selected_base": cand_base,
            "candidate_selected_stress": cand_stress,
            "candidate_seed_base": seed_base,
            "control_masked_base": control_base,
            "control_masked_stress": control_stress,
            "control_raw_base": control_raw,
            "per_window": per_window,
        },
        "neighbors": neighbor_rows,
        "candle_coverage": coverage,
        "incumbent_legs": {w: incumbent_raw[w] for w in sorted(incumbent_raw)},
        "incumbent_bars": {f"{w} | {ds}": b for (w, ds), b in sorted(bars.items())},
        "mask_audit": mask_audit(reg, selected, HELD_OUT_WINDOWS),
        "verdict": v,
        "close_validation": aggregate_close_validations(
            [l.get("close_validation") for l in cand_legs if l.get("close_validation")]),
    }


def run_parity(db_path: str) -> dict:
    import pandas as pd
    from data_fetcher import load_cached_data
    from parity_diff import ParityConfig, compute_parity_frame, summarize as parity_summary
    with open(CANDIDATE_SEED) as fh:
        params = json.load(fh)["params"]
    out = {}
    for symbol, tf, start, end in (("BTC/USDT", "1h", "2026-08-20", "2026-10-01"),
                                   ("ETH/USDT", "4h", "2026-05-01", "2026-10-01"),
                                   ("SOL/USDT", "1h", "2026-08-20", "2026-10-01")):
        df = load_cached_data(symbol, tf, start_date=start, end_date=end)
        df = df[df.index < pd.Timestamp(end)]
        for window in (None, 200):
            for batched in (False, True):
                cfg = ParityConfig(strategy_name=CANDIDATE, params=dict(params), registry="futures",
                                   platform="hyperliquid", symbol=symbol, timeframe=tf,
                                   close_refs=CLOSE_STRATEGIES, direction=DIRECTION,
                                   batched=batched, funding_mode=FUNDING_MODE,
                                   stop_kwargs={"stop_loss_atr_mult": STOP_LOSS_ATR_MULT})
                frame = compute_parity_frame(df, cfg=cfg, window=window)
                s = parity_summary(frame)
                s.pop("close_validation", None)
                s["live_open_action_bars"] = (int((frame["live_open_action"] != "none").sum())
                                              if len(frame) else 0)
                out[f"{symbol} {tf} window={window or 0} batched={batched}"] = s
    return out


def _fmt(v, nd=2):
    if v is None:
        return "-"
    if isinstance(v, bool):
        return "yes" if v else "no"
    if isinstance(v, float):
        return f"{v:.{nd}f}"
    return str(v)


def render(result: dict) -> str:
    v = result["verdict"]
    p = result["protocol"]
    h = result["held_out"]
    lines = [
        "# Ultimate Oscillator breakout confirmation: held-out result (#1664)",
        "",
        f"Verdict: **{v['outcome'].upper()}**. {v['reason']}. {v['live_status']}.",
        "",
        "Generated by `run_study.py` from `results.json`; do not edit by hand.",
        "",
        "## Inputs and execution assumptions",
        "",
        f"- Frozen inputs `{result['frozen_inputs']['path']}` sha256 `{result['frozen_inputs']['sha256']}`; "
        "every file hash is checked before a run, and the run builds a private SQLite cache from them with "
        "network access disabled.",
        f"- Candles: {result['frozen_inputs']['candle_source']}. They stand in for Hyperliquid perpetual "
        "prices; basis between the two venues is not modelled.",
        f"- Funding: {result['frozen_inputs']['funding_source']}, charged hourly on open exposure "
        f"(`--funding {p['funding_mode']}`).",
        f"- Fees: Hyperliquid taker {p['taker_fee_pct'] * 100:.3f}% per side. Slippage "
        f"{p['base_slippage_pct'] * 100:.3f}% per side stands in for spread (stress leg "
        f"{p['stress_slippage_pct'] * 100:.3f}%). Lot size and venue minimum order size are not modelled on "
        "this ordinary M1 path.",
        "- Datasets: " + ", ".join(f"`{d}`" for d in p["datasets"]) + ".",
    ]
    for name, (start, end) in p["windows"].items():
        role = ("selection" if name == p["selection_window"] else "held-out")
        lines.append(f"- Window `{name}` [{start}, {end}): {role}")
    lines.append("- Each frame starts at its window start, so the first required-history bars of every "
                 "window hold with `insufficient_history` for the candidate and its masked control; "
                 "incumbents see the same bars.")
    for f in result["frozen_inputs"]["files"]:
        lines.append(f"- `{f['path']}` rows {f.get('rows', '-')} sha256 `{f['sha256'][:16]}`")
    lines += [
        "",
        "## Protocol (frozen in `study_spec.json` before held-out results were read)",
        "",
        f"- Candidate `{CANDIDATE}`, direction `{p['direction']}`, close owner `tiered_tp_pct` (one tier: "
        f"{CLOSE_STRATEGIES[0]['params']['tp_tiers'][0]['profit_pct'] * 100:.0f}% profit closes the full "
        f"position), sole stop owner fixed entry ATR x{p['stop_loss_atr_mult']}, strict close comparison, "
        f"`{p['intrabar_resolution']}` intrabar policy, capital ${p['capital']:.0f} per leg, next-bar-open fills.",
        f"- Selection on `{p['selection_window']}` over the {result['train']['grid_size']}-combination "
        f"optimizer grid: {p['spec']['selection_rule']}",
        f"- Paired control: `{CONTROL}` with {p['control']['params_rule']}; {p['control']['mask']}. "
        "The same close, stop, direction, capital, costs and data apply.",
        f"- Primary comparisons: the M1 incumbent bar (median of {len(p['incumbents'])} incumbents per "
        "window and dataset; incumbents run long-only without a close strategy) and the masked control.",
        f"- Sample rule: {p['spec']['sample_rule']}",
        f"- Verdict rule: {p['spec']['verdict_rule']}",
        "",
        "## Selection",
        "",
        f"- Rule outcome: {result['train']['selection']['rule_outcome']}; eligible combinations "
        f"{result['train']['selection'].get('eligible')}; selected `{json.dumps(result['selected_params'], sort_keys=True)}`.",
    ]
    ranked = sorted(result["train"]["rows"], key=lambda r: -r["summary"]["mean_sharpe"])
    lines += ["", "Top 10 training combinations by mean Sharpe (all rows are in `results.json`):", "",
              "| Params | Mean Sharpe | Mean DDadj | Net $ | Positions | Liquidated legs |",
              "|---|---|---|---|---|---|"]
    for r in ranked[:10]:
        s = r["summary"]
        lines.append(f"| {r['label']} | {_fmt(s['mean_sharpe'], 3)} | {_fmt(s['mean_ddadj'], 3)} | "
                     f"{_fmt(s['net_pnl_usd'])} | {s['positions']} | {s['liquidated_legs']} |")
    pos_train = sum(1 for r in result["train"]["rows"] if r["summary"]["net_pnl_usd"] > 0)
    lines.append("")
    lines.append(f"Training combinations with positive net PnL: {pos_train}/{len(result['train']['rows'])}.")
    lines += [
        "",
        "## Held-out comparison (four windows x six datasets, pooled)",
        "",
        "| Arm | Mean ret % | Mean Sharpe | Mean DDadj | Worst DD % | Net $ | Fees $ | Funding $ | Turnover | "
        "Positions (L/S) | Clusters | Long $ | Short $ | Top leg share | Top-5 share | Funding complete |",
        "|---|---|---|---|---|---|---|---|---|---|---|---|---|---|---|---|",
    ]
    arms = (("candidate selected, cost x1", h["candidate_selected_base"]),
            ("candidate selected, slippage x2", h["candidate_selected_stress"]),
            ("candidate seed, cost x1", h["candidate_seed_base"]),
            ("control masked, cost x1", h["control_masked_base"]),
            ("control masked, slippage x2", h["control_masked_stress"]),
            ("control raw, cost x1", h["control_raw_base"]))
    for label, s in arms:
        lines.append(
            f"| {label} | {_fmt(s['mean_return_pct'])} | {_fmt(s['mean_sharpe'], 3)} | {_fmt(s['mean_ddadj'], 3)} | "
            f"{_fmt(s['worst_max_dd_pct'])} | {_fmt(s['net_pnl_usd'])} | {_fmt(s['fees_usd'])} | "
            f"{_fmt(s['funding_pnl_usd'])} | {_fmt(s['mean_turnover'])} | "
            f"{s['positions']} ({s['long_positions']}/{s['short_positions']}) | {s['independent_clusters']} | "
            f"{_fmt(s['long_net_usd'])} | {_fmt(s['short_net_usd'])} | {_fmt(s['top_leg_abs_pnl_share'])} | "
            f"{_fmt(s['top5_position_abs_pnl_share'])} | {_fmt(s['funding_complete'])} |")
    c = h["candidate_selected_base"]
    lines += ["",
              f"M1 incumbent bar over the same 24 legs: mean Sharpe {_fmt(c.get('bar_mean_sharpe'), 3)}, "
              f"mean DDadj {_fmt(c.get('bar_mean_ddadj'), 3)}; the candidate beats the bar on Sharpe in "
              f"{c.get('beats_bar_sharpe_legs')}/{c['legs']} legs and on DDadj in "
              f"{c.get('beats_bar_ddadj_legs')}/{c['legs']} legs.",
              "", "## Per window (selected, cost x1)", "",
              "| Window | Candidate Sharpe | Control Sharpe | Candidate DDadj | Control DDadj | Candidate net $ | "
              "Control net $ | Candidate positions (L/S) | Bar Sharpe | Bar DDadj |",
              "|---|---|---|---|---|---|---|---|---|---|"]
    for w, row in h["per_window"].items():
        cw, kw = row["candidate"], row["control_masked"]
        lines.append(f"| {w} | {_fmt(cw['mean_sharpe'], 3)} | {_fmt(kw['mean_sharpe'], 3)} | "
                     f"{_fmt(cw['mean_ddadj'], 3)} | {_fmt(kw['mean_ddadj'], 3)} | {_fmt(cw['net_pnl_usd'])} | "
                     f"{_fmt(kw['net_pnl_usd'])} | {cw['positions']} ({cw['long_positions']}/{cw['short_positions']}) | "
                     f"{_fmt(cw.get('bar_mean_sharpe'), 3)} | {_fmt(cw.get('bar_mean_ddadj'), 3)} |")
    lines += ["", "## Candidate per window and dataset (selected, cost x1)", "",
              "| Leg | Ret % | Sharpe | Max DD % | Net $ | Fees $ | Funding $ | Positions (L/S) | Long $ | Short $ | "
              "Turnover | Funding complete |",
              "|---|---|---|---|---|---|---|---|---|---|---|---|"]
    for key, s in h["candidate_selected_base"]["per_leg"].items():
        lines.append(f"| {key} | {_fmt(s['return_pct'])} | {_fmt(s['sharpe'], 3)} | {_fmt(s['max_dd_pct'])} | "
                     f"{_fmt(s['net_pnl_usd'])} | {_fmt(s['fees_usd'])} | {_fmt(s['funding_pnl_usd'])} | "
                     f"{s['positions']} ({s['long_positions']}/{s['short_positions']}) | {_fmt(s['long_net_usd'])} | "
                     f"{_fmt(s['short_net_usd'])} | {_fmt(s['turnover'])} | {_fmt(s['funding_complete'])} |")
    lines += ["", "## Neighbouring-parameter stability (held-out, cost x1, each with its own masked control)", "",
              "| Variant | Train Sharpe | Held Sharpe | Held DDadj | Held net $ | Positions | Control Sharpe | "
              "Control DDadj | Net positive | Beats control on both |",
              "|---|---|---|---|---|---|---|---|---|---|"]
    for r in result["neighbors"]:
        lines.append(f"| {r['label']} | {_fmt(r['train_mean_sharpe'], 3)} | {_fmt(r['held_mean_sharpe'], 3)} | "
                     f"{_fmt(r['held_mean_ddadj'], 3)} | {_fmt(r['held_net_pnl_usd'])} | {r['held_positions']} | "
                     f"{_fmt(r['control_mean_sharpe'], 3)} | {_fmt(r['control_mean_ddadj'], 3)} | "
                     f"{_fmt(r['net_positive'])} | {_fmt(r['beats_control_sharpe'] and r['beats_control_ddadj'])} |")
    lines += ["", f"Stable neighbours: {v['stable_neighbors']}/{v['evaluable_neighbors']} "
              f"(rule: at least two thirds).", "", "## Verdict checks", ""]
    for k, ok in sorted(v["checks"].items()):
        lines.append(f"- {k}: {_fmt(ok)}")
    lines += ["", "## Coverage", ""]
    for key, cov in result["candle_coverage"].items():
        if cov["missing_bars"] or cov["gaps"] or not cov["complete"]:
            lines.append(f"- Candles `{key}`: {cov['present_bars']}/{cov['expected_bars']} bars, gaps "
                         f"{cov['gaps']}, counted complete: {_fmt(cov['complete'])}")
    lines.append("- Every other window and dataset has every expected candle.")
    for key, s in h["candidate_selected_base"]["per_leg"].items():
        if not s["funding_complete"]:
            lines.append(f"- Funding incomplete on `{key}`")
    lines.append("- Funding coverage per leg (rows, first, last, sha256) is recorded in `results.json`.")
    lines += ["", "## Masked-control audit (selected parameters, held-out)", "",
              "| Leg | Bars | Valid bars | Raw events | Masked events | Candidate entries (L/S) | Mask = validity | "
              "Masked entries are raw events | Candidate entries within masked |",
              "|---|---|---|---|---|---|---|---|---|"]
    for key, a in result["mask_audit"].items():
        lines.append(f"| {key} | {a['bars']} | {a['valid_bars']} | {a['raw_events']} | {a['masked_events']} | "
                     f"{a['candidate_entries']} ({a['candidate_long']}/{a['candidate_short']}) | "
                     f"{_fmt(a['mask_equals_validity'])} | {_fmt(a['masked_entries_are_raw_events'])} | "
                     f"{_fmt(a['candidate_entries_subset_of_masked'])} |")
    if result.get("parity"):
        lines += ["", "## Decision parity on frozen frames (seed parameters, explicit paper batch evidence)", "",
                  "| Run | Bars compared | Mismatches | Batch mismatches | Close parity | Clean |",
                  "|---|---|---|---|---|---|"]
        for key, s in result["parity"].items():
            lines.append(f"| {key} | {s['bars_compared']} | {s['mismatches']} | {s.get('batch_mismatches', '-')} | "
                         f"{s['close_parity']} | {_fmt(s['clean'])} |")
    lines += ["", "## Source hashes", ""]
    for path, digest in sorted(result["source_sha256"].items()):
        lines.append(f"- `{path}` `{digest}`")
    lines.append("")
    return "\n".join(lines)


def main(argv=None) -> int:
    ap = argparse.ArgumentParser(description="#1664 frozen held-out study driver (research-only, suggest-only)")
    ap.add_argument("--freeze", metavar="SOURCE_DB",
                    help="Refresh SOURCE_DB (a scratch copy of the OHLCV cache) from the network and "
                         "write the frozen inputs under data/")
    ap.add_argument("--json", default=os.path.join(_HERE, "results.json"), dest="json_out")
    ap.add_argument("--report", default=os.path.join(_HERE, "REPORT.md"))
    ap.add_argument("--render-only", action="store_true",
                    help="Re-render the report from the committed results JSON")
    ap.add_argument("--parity", action="store_true",
                    help="Run decision parity on frozen frames and store it in the results JSON")
    ap.add_argument("--workers", type=int, default=max(1, (os.cpu_count() or 2) - 1))
    args = ap.parse_args(argv)

    if args.freeze:
        os.environ[CACHE_ENV] = os.path.abspath(args.freeze)
        manifest = freeze(os.path.abspath(args.freeze))
        print(json.dumps({f["path"]: f.get("rows") for f in manifest["files"]}, indent=1))
        return 0
    if args.render_only:
        with open(args.json_out) as fh:
            result = json.load(fh)
    else:
        frozen = load_frozen_inputs()
        with tempfile.TemporaryDirectory(prefix="uo1664_") as tmp:
            db_path = os.path.join(tmp, "frozen_cache.db")
            os.environ[CACHE_ENV] = db_path
            build_frozen_cache(frozen, db_path)
            forbid_network()
            install_result_capture()
            if args.parity:
                with open(args.json_out) as fh:
                    result = json.load(fh)
                result["parity"] = run_parity(db_path)
            else:
                runner = Runner(db_path, args.workers)
                try:
                    result = run(runner, frozen)
                finally:
                    runner.close()
        with open(args.json_out, "w") as fh:
            json.dump(result, fh, indent=1, sort_keys=True, default=str)
            fh.write("\n")
    with open(args.report, "w") as fh:
        fh.write(render(result))
    print(f"verdict: {result['verdict']['outcome']} ({result['verdict']['reason']})")
    print(f"wrote {args.json_out} and {args.report}")
    return 0


if __name__ == "__main__":
    sys.exit(main())

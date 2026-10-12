#!/usr/bin/env python3

import argparse
import copy
import csv
import gzip
import hashlib
import io
import json
import os
import sqlite3
import statistics
import subprocess
import sys
import time

HERE = os.path.dirname(os.path.abspath(__file__))
BACKTEST = os.path.abspath(os.path.join(HERE, "..", ".."))
REPO = os.path.abspath(os.path.join(BACKTEST, ".."))
DATA = os.path.join(HERE, "data")
PROTOCOL_PATH = os.path.join(HERE, "protocol.json")
MANIFEST_PATH = os.path.join(HERE, "study_manifest.json")
RESULTS_PATH = os.path.join(HERE, "results.json")
REPORT_PATH = os.path.join(HERE, "REPORT.md")
CACHE_ENV = "GO_TRADER_OHLCV_CACHE_DB"
HOUR_MS = 3_600_000
TF_MS = {"1h": 3_600_000, "4h": 14_400_000}


def sha256_bytes(data: bytes) -> str:
    return hashlib.sha256(data).hexdigest()


def sha256_file(path: str) -> str:
    with open(path, "rb") as fh:
        return sha256_bytes(fh.read())


def load_protocol() -> dict:
    with open(PROTOCOL_PATH) as fh:
        return json.load(fh)


def to_ms(day: str) -> int:
    import pandas as pd
    return int(pd.Timestamp(day, tz="UTC").timestamp() * 1000)


def coin_of(symbol: str) -> str:
    return symbol.split("/")[0]


def candle_file(symbol: str, tf: str) -> str:
    return f"{coin_of(symbol)}_{tf}_candles.csv.gz"


def funding_file(coin: str) -> str:
    return f"{coin}_funding.csv.gz"


def write_gz_csv(path: str, header: list, rows: list) -> None:
    buf = io.StringIO()
    w = csv.writer(buf, lineterminator="\n")
    w.writerow(header)
    w.writerows(rows)
    raw = buf.getvalue().encode()
    with open(path, "wb") as fh:
        with gzip.GzipFile(filename="", mode="wb", fileobj=fh, mtime=0) as gz:
            gz.write(raw)


def read_gz_csv(path: str) -> list:
    with gzip.open(path, "rt") as fh:
        rows = list(csv.reader(fh))
    return rows[1:]


def git_head() -> str:
    try:
        head = subprocess.check_output(["git", "-C", REPO, "rev-parse", "HEAD"], text=True).strip()
        dirty = subprocess.check_output(["git", "-C", REPO, "status", "--porcelain", "--untracked-files=no"],
                                        text=True).strip()
    except (OSError, subprocess.CalledProcessError):
        return "unknown"
    return head + ("+uncommitted-changes" if dirty else "")


def set_cache(path: str) -> None:
    os.environ[CACHE_ENV] = os.path.abspath(path)
    for p in (BACKTEST, os.path.join(REPO, "shared_tools")):
        if p not in sys.path:
            sys.path.insert(0, p)


def missing_funding_hours(cache_db: str, coin: str, start_ms: int, end_ms: int) -> list:
    conn = sqlite3.connect(cache_db)
    try:
        priced = {int(t) // HOUR_MS * HOUR_MS for (t,) in conn.execute(
            "SELECT timestamp FROM funding_rates WHERE exchange='hyperliquid' AND coin=? AND timestamp >= ?"
            " AND timestamp < ?", (coin, start_ms, end_ms))}
        absent = {int(t) for (t,) in conn.execute(
            "SELECT timestamp FROM funding_venue_gaps WHERE exchange='hyperliquid' AND coin=? AND timestamp >= ?"
            " AND timestamp < ?", (coin, start_ms, end_ms))}
    finally:
        conn.close()
    return [h for h in range(start_ms, end_ms, HOUR_MS) if h not in priced and h not in absent]


def acquire(cache_db: str, resume: bool) -> None:
    proto = load_protocol()
    if os.path.exists(cache_db) and not resume:
        raise SystemExit(f"refusing to reuse an existing cache {cache_db}; acquisition starts empty "
                         "(--resume continues an interrupted acquisition into the same cache)")
    set_cache(cache_db)
    import pandas as pd
    from data_fetcher import fetch_full_history
    from funding_fetcher import load_cached_funding
    import storage

    start_ms, end_ms = to_ms(proto["acquisition_start"]), to_ms(proto["pinned_end"])
    os.makedirs(DATA, exist_ok=True)
    files = {}
    for symbol in proto["symbols"]:
        for tf in proto["groups"]:
            df = storage.load_ohlcv("binanceus", symbol, tf, start_ms, end_ms - 1, db_path=cache_db)
            if not resume or df.empty or int(df["timestamp"].iloc[-1]) < end_ms - TF_MS[tf]:
                fetch_full_history(symbol, tf, since=proto["acquisition_start"], store=True)
                df = storage.load_ohlcv("binanceus", symbol, tf, start_ms, end_ms - 1, db_path=cache_db)
            rows = [[int(r.timestamp), repr(float(r.open)), repr(float(r.high)), repr(float(r.low)),
                     repr(float(r.close)), repr(float(r.volume))] for r in df.itertuples()]
            path = os.path.join(DATA, candle_file(symbol, tf))
            write_gz_csv(path, ["timestamp", "open", "high", "low", "close", "volume"], rows)
            ts = [r[0] for r in rows]
            steps = [b - a for a, b in zip(ts, ts[1:])]
            files[candle_file(symbol, tf)] = {
                "kind": "candles", "exchange": "binanceus", "symbol": symbol, "timeframe": tf,
                "rows": len(rows), "first_ms": rows[0][0], "last_ms": rows[-1][0],
                "gap_count": sum(1 for d in steps if d != TF_MS[tf]),
                "missing_bars": sum(d // TF_MS[tf] - 1 for d in steps if d != TF_MS[tf]),
                "sha256": sha256_file(path),
            }
    for symbol in proto["symbols"]:
        coin = coin_of(symbol)
        chunk_ms = 30 * 24 * HOUR_MS
        cursor = start_ms
        while cursor < end_ms:
            stop = min(cursor + chunk_ms, end_ms)
            for attempt in range(8):
                box = {}
                load_cached_funding(coin, pd.Timestamp(cursor, unit="ms", tz="UTC"),
                                    end_date=pd.Timestamp(stop, unit="ms", tz="UTC"),
                                    db_path=cache_db, source_box=box)
                if not box.get("fetch_error"):
                    break
                print(f"{coin} funding {cursor}..{stop}: {box['fetch_error'][:120]}; retry {attempt + 1}",
                      flush=True)
                time.sleep(15 * (attempt + 1))
            else:
                raise SystemExit(f"{coin} funding {cursor}..{stop} could not be fetched")
            cursor = stop
        missing = missing_funding_hours(cache_db, coin, start_ms, end_ms)
        first_rate = storage.load_funding_first_ts("hyperliquid", coin, db_path=cache_db)
        late = [h for h in missing if first_rate is None or h + HOUR_MS > first_rate]
        if late:
            raise SystemExit(f"{coin} funding has {len(late)} hours after the venue's first rate with neither a "
                             f"rate nor a recorded venue-absent mark, first {late[:3]}")
        pre_venue = len(missing)
        conn = sqlite3.connect(cache_db)
        try:
            rates = conn.execute(
                "SELECT timestamp, rate FROM funding_rates WHERE exchange='hyperliquid' AND coin=?"
                " AND timestamp >= ? AND timestamp < ? ORDER BY timestamp", (coin, start_ms, end_ms)).fetchall()
            gaps = conn.execute(
                "SELECT timestamp, observed_through_ms FROM funding_venue_gaps WHERE exchange='hyperliquid'"
                " AND coin=? AND timestamp >= ? AND timestamp < ? ORDER BY timestamp",
                (coin, start_ms, end_ms)).fetchall()
            coverage = conn.execute(
                "SELECT start_ts, end_ts FROM funding_coverage WHERE exchange='hyperliquid' AND coin=?"
                " ORDER BY start_ts", (coin,)).fetchall()
        finally:
            conn.close()
        path = os.path.join(DATA, funding_file(coin))
        write_gz_csv(path, ["timestamp", "rate"], [[int(t), repr(float(r))] for t, r in rates])
        clipped = [[start_ms, end_ms]]
        files[funding_file(coin)] = {
            "kind": "funding", "exchange": "hyperliquid", "coin": coin, "rows": len(rates),
            "first_ms": int(rates[0][0]) if rates else None, "last_ms": int(rates[-1][0]) if rates else None,
            "coverage": clipped,
            "stored_coverage_intervals": [[int(s), int(e)] for s, e in coverage],
            "pre_venue_hours": pre_venue,
            "first_venue_rate_ms": first_rate,
            "venue_absent_hours": [[int(t), int(o)] for t, o in gaps],
            "sha256": sha256_file(path),
        }
    manifest = {
        "study": proto["study"],
        "acquired_at_utc": time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime()),
        "source_commit": git_head(),
        "candles": "ccxt binanceus fetch_ohlcv through shared_tools/data_fetcher.fetch_full_history; closed bars "
                   "with opening time >= acquisition_start and < pinned_end",
        "funding": "Hyperliquid fundingHistory through shared_tools/funding_fetcher.load_cached_funding in 30-day "
                   "chunks; rows with timestamp >= acquisition_start and < pinned_end. Every hour after the venue's first "
                   "rate has a rate or a strict-download venue-absent mark; the pre_venue_hours before it have "
                   "neither and stay unpriced (the helper never refills hours it cannot bracket), so a leg holding "
                   "a position then is funding-incomplete. The frozen coverage is the whole span; "
                   "stored_coverage_intervals records the helper's own intervals",
        "acquisition_start": proto["acquisition_start"],
        "pinned_end": proto["pinned_end"],
        "files": dict(sorted(files.items())),
    }
    with open(MANIFEST_PATH, "w") as fh:
        json.dump(manifest, fh, indent=2, sort_keys=True)
        fh.write("\n")
    print(f"wrote {len(files)} frozen files and {MANIFEST_PATH}")


def verify_frozen() -> dict:
    with open(MANIFEST_PATH) as fh:
        manifest = json.load(fh)
    for name, meta in manifest["files"].items():
        got = sha256_file(os.path.join(DATA, name))
        if got != meta["sha256"]:
            raise SystemExit(f"frozen input {name} sha256 {got} != manifest {meta['sha256']}")
    return manifest


def build_cache(cache_db: str, manifest: dict) -> None:
    if os.path.exists(cache_db):
        raise SystemExit(f"refusing to reuse an existing cache {cache_db}; the run rebuilds it from data/")
    set_cache(cache_db)
    import pandas as pd
    import storage
    storage.init_db(cache_db)
    for name, meta in manifest["files"].items():
        rows = read_gz_csv(os.path.join(DATA, name))
        if meta["kind"] == "candles":
            df = pd.DataFrame(rows, columns=["timestamp", "open", "high", "low", "close", "volume"])
            df = df.astype({"timestamp": "int64", "open": float, "high": float, "low": float,
                            "close": float, "volume": float})
            storage.store_ohlcv(df, meta["exchange"], meta["symbol"], meta["timeframe"], db_path=cache_db)
        else:
            records = [{"time": int(t), "rate": float(r)} for t, r in rows]
            absent = [h for h, _ in meta["venue_absent_hours"]]
            observed = max((o for _, o in meta["venue_absent_hours"]), default=None)
            storage.store_funding_fetch(meta["exchange"], meta["coin"], records, absent,
                                        observed_through_ms=observed, db_path=cache_db)
            for s, e in meta["coverage"]:
                storage.store_funding_coverage(meta["exchange"], meta["coin"], s, e, db_path=cache_db)


def forbid_network() -> None:
    import data_fetcher
    import funding_fetcher

    def refuse(*_a, **_k):
        raise RuntimeError("frozen study run: network acquisition is disabled")

    data_fetcher.fetch_full_history = refuse
    funding_fetcher._hl_adapter = refuse


class Capture:
    def __init__(self):
        self.results = []

    def install(self):
        import backtester
        base = backtester.Backtester
        sink = self

        class CapturingBacktester(base):
            def run(self, *a, **k):
                out = super().run(*a, **k)
                sink.results.append(out)
                return out

        backtester.Backtester = CapturingBacktester

    def take(self):
        if not self.results:
            return None
        out = self.results[-1]
        self.results.clear()
        return out


def frame_sha256(symbol: str, tf: str, window: tuple) -> dict:
    import pandas as pd
    from data_fetcher import load_cached_data
    start, end = window
    df = load_cached_data(symbol, tf, start_date=start, end_date=end)
    df = df[df.index < pd.Timestamp(end)]
    body = df[["timestamp", "open", "high", "low", "close", "volume"]].to_csv(index=False).encode()
    return {"rows": int(len(df)), "first": str(df.index[0]), "last": str(df.index[-1]),
            "sha256": sha256_bytes(body)}


def candidate_spec(proto: dict, name: str, params: dict) -> dict:
    return {
        "name": name,
        "type": proto["strategy_type"],
        "params": dict(params),
        "direction": proto["direction"],
        "close_strategies": copy.deepcopy(proto["close_strategies"]),
        "stop_loss_atr_mult": proto["stop_loss_atr_mult"],
    }


def grid_combos(grid: dict) -> list:
    import itertools
    keys = list(grid)
    return [dict(zip(keys, vals)) for vals in itertools.product(*(grid[k] for k in keys))]


def funding_coverage_counts(cov: dict) -> dict:
    present = cov.get("present_hours")
    absent = cov.get("venue_absent_hours")
    priced = present - absent if present is not None and absent is not None else None
    return {"expected_hours": cov.get("expected_hours"), "present_hours": present, "priced_hours": priced,
            "missing_hours": cov.get("missing_hours"), "venue_absent_hours": absent}


def leg_summary(leg, results, capital: float) -> dict:
    from eval_windows import execution_metrics
    if leg is None:
        return None
    ex = execution_metrics(results, capital) if results is not None else {}
    positions = ex.get("position_list") or []
    entry_notional = [p["entry_notional"] for p in positions if p["entry_notional"] > 0]
    pos_pnl = [p["net_pnl"] for p in positions]
    block = leg.get("funding") or {}
    cov = block.get("coverage") or {}
    return {
        "sharpe": leg["sharpe"], "return_pct": leg["return_pct"], "max_dd_pct": leg["max_dd_pct"],
        "ddadj": leg["ddadj"], "trades": leg["trades"], "bh_return_pct": leg["bh_return_pct"],
        "liquidated": leg["liquidated"], "funding_incomplete": bool(leg.get("funding_incomplete")),
        "positions": ex.get("positions", 0), "long_positions": ex.get("long_positions", 0),
        "short_positions": ex.get("short_positions", 0), "fees_usd": ex.get("fees_usd", 0.0),
        "funding_pnl_usd": ex.get("funding_pnl_usd", 0.0), "net_pnl_usd": ex.get("net_pnl_usd", 0.0),
        "traded_notional_usd": ex.get("traded_notional_usd", 0.0), "turnover": ex.get("turnover"),
        "bars_in_market": ex.get("bars_in_market", 0),
        "mean_entry_notional_usd": round(statistics.mean(entry_notional), 4) if entry_notional else None,
        "max_entry_notional_usd": round(max(entry_notional), 4) if entry_notional else None,
        "position_net_pnl": [round(x, 6) for x in pos_pnl],
        "exit_reasons": sorted({r.split(":")[0] for p in positions for r in p["exit_reasons"] if r}),
        "funding": {
            "mode": block.get("mode"), "source": block.get("source"), "available": block.get("available"),
            "complete": block.get("complete"), "sha256": block.get("sha256"), "row_count": block.get("row_count"),
            "first": block.get("first"), "last": block.get("last"),
            **funding_coverage_counts(cov), "frame_complete": cov.get("complete"),
            "unpriced_held_hours": block.get("unpriced_held_hours"),
            "fetch_error": block.get("fetch_error"), "error": block.get("error"),
        },
    }


class Runner:
    def __init__(self, proto: dict):
        import eval_windows as ew
        from registry_loader import load_registry
        self.proto = proto
        self.ew = ew
        for name, (s, e) in proto["windows"].items():
            ew.WINDOWS[name] = (s, e)
        self.capital = proto["capital_usd"]
        self.reg = load_registry(proto["registry"])
        self.capture = Capture()
        self.capture.install()
        self.bars = {g: {} for g in proto["groups"]}
        self.frames = {}

    def datasets(self, group: str) -> list:
        return [(s, group) for s in self.proto["symbols"]]

    def params_for(self, name: str, group: str, params: dict) -> dict:
        if name == self.proto["candidate"]:
            return dict(params, expected_interval_ms=self.proto["groups"][group]["expected_interval_ms"])
        return dict(params)

    def evaluate(self, name: str, params: dict, group: str, window: str) -> dict:
        ew = self.ew
        spec = candidate_spec(self.proto, name, self.params_for(name, group, params))
        ew.validate_candidate(spec)
        win = ew.WINDOWS[window]
        if window not in self.bars[group]:
            self.bars[group][window] = ew.incumbent_bars(ew.compute_incumbent_legs(
                self.reg, self.datasets(group), win, self.capital, funding_mode=self.proto["funding_mode"]))
        self.capture.results.clear()
        legs = {}
        for symbol, tf in self.datasets(group):
            leg = ew.run_candidate_leg(self.reg, spec, symbol, tf, win, capital=self.capital,
                                       funding_mode=self.proto["funding_mode"])
            legs[ew.dataset_key(symbol, tf)] = leg_summary(leg, self.capture.take(), self.capital)
            fkey = (symbol, tf, window)
            if fkey not in self.frames:
                self.frames[fkey] = frame_sha256(symbol, tf, win)
        raw_legs = {ds: ({k: v for k, v in s.items() if k in ("sharpe", "return_pct", "max_dd_pct", "ddadj",
                                                                "trades", "bh_return_pct", "liquidated",
                                                                "funding_incomplete")} if s else None)
                    for ds, s in legs.items()}
        score = ew.score_candidate(raw_legs, self.bars[group][window])
        return {"name": name, "params": spec["params"], "group": group, "window": window,
                "window_range": list(win), "verdict": score["verdict"],
                "mean_sharpe": score.get("mean_sharpe"), "mean_ddadj": score.get("mean_ddadj"),
                "mean_bar_sharpe": score.get("mean_bar_sharpe"), "mean_bar_ddadj": score.get("mean_bar_ddadj"),
                "traded_datasets": score.get("traded_datasets"), "beats_sharpe_count": score.get("beats_sharpe_count"),
                "beats_ddadj_count": score.get("beats_ddadj_count"), "degenerate": score.get("degenerate"),
                "bars": self.bars[group][window], "legs": legs}


def select(rows: list, rule: dict, grid_keys: list, fallback: dict) -> dict:
    eligible = []
    for r in rows:
        legs = [l for l in r["legs"].values() if l is not None]
        if len(legs) != len(r["legs"]) or r["verdict"] == "funding_incomplete":
            continue
        if any(l["trades"] < rule["min_training_trades"] for l in legs):
            continue
        eligible.append(r)
    if not eligible:
        return {"params": dict(fallback), "how": "seed_fallback", "eligible": 0}
    key = lambda r: (-r["mean_ddadj"], -r["mean_sharpe"]) + tuple(r["params"][k] for k in grid_keys)
    best = sorted(eligible, key=key)[0]
    return {"params": {k: best["params"][k] for k in grid_keys}, "how": "selected", "eligible": len(eligible),
            "mean_ddadj": best["mean_ddadj"], "mean_sharpe": best["mean_sharpe"]}


def _window_unscorable(row: dict, comps: dict, window: str) -> list:
    bad = []
    for ds, l in row["legs"].items():
        if l is None or l["funding_incomplete"]:
            bad.append(f"candidate {ds}")
    for ds, bar in (row["bars"] or {}).items():
        if bar is None or bar.get("funding_incomplete"):
            bad.append(f"incumbent median {ds}")
    for cname, rows in comps.items():
        for ds, l in rows[window]["legs"].items():
            if l is None or l["funding_incomplete"]:
                bad.append(f"{cname} {ds}")
    return bad


def group_verdict(proto: dict, conf: dict, comps: dict) -> dict:
    rules = proto["verdict_rules"]
    held = proto["held_out_windows"]
    unscorable = {w: _window_unscorable(row, comps, w) for w, row in conf.items()}
    scorable = [w for w in conf if not unscorable[w]]
    cand_legs = [l for w in scorable for l in conf[w]["legs"].values() if l is not None]
    positions = sum(l["positions"] for l in cand_legs)
    any_unscorable = len(scorable) < len(conf)
    net = sum(l["net_pnl_usd"] for l in cand_legs)
    pos_pnl = [x for l in cand_legs for x in l["position_net_pnl"]]
    positive = sum(x for x in pos_pnl if x > 0)
    top_share = (max(pos_pnl) / positive) if pos_pnl and positive > 0 else None
    mean_ret = statistics.mean(l["return_pct"] for l in cand_legs) if cand_legs else None
    comp_ret = {c: statistics.mean(l["return_pct"] for w in scorable for l in rows[w]["legs"].values() if l)
                for c, rows in comps.items()} if scorable else {}
    held_pass = sum(1 for w in held if w in scorable and conf[w]["verdict"] == "pass")
    held_unscorable = sum(1 for w in held if w not in scorable)

    def tri(value, known):
        return value if known else None

    conditions = {
        "oos_pass": tri(conf["oos"]["verdict"] == "pass", "oos" in scorable),
        "held_out_passes": True if held_pass >= 2 else (False if held_pass + held_unscorable < 2 else None),
        "net_pnl_positive": tri(net > 0, not any_unscorable),
        "beats_comparators": tri(mean_ret is not None and all(mean_ret > v for v in comp_ret.values()),
                                 not any_unscorable),
        "not_concentrated": tri(top_share is not None and top_share <= 0.25, not any_unscorable),
    }
    checks = {
        "scorable_windows": scorable,
        "unscorable_windows": {w: v for w, v in unscorable.items() if v},
        "conditions": conditions,
        "held_out_passes": held_pass,
        "net_pnl_usd": round(net, 4),
        "mean_return_pct": round(mean_ret, 4) if mean_ret is not None else None,
        "comparator_mean_return_pct": {c: round(v, 4) for c, v in comp_ret.items()},
        "top_position_share": round(top_share, 4) if top_share is not None else None,
        "positions": positions,
    }
    false = [k for k, v in conditions.items() if v is False]
    unknown = [k for k, v in conditions.items() if v is None]
    if positions < rules["min_confirmation_positions"]:
        verdict, why = "inconclusive", (f"{positions} positions in scorable windows < "
                                        f"{rules['min_confirmation_positions']}")
    elif false:
        verdict, why = "fail", "false: " + ", ".join(false)
    elif not unknown:
        verdict, why = "pass", "every condition true"
    else:
        verdict, why = "inconclusive", "unknown: " + ", ".join(unknown)
    return {"verdict": verdict, "reason": why, "checks": checks}


def overall_verdict(groups: dict) -> str:
    vs = {g["verdict"] for g in groups.values()}
    if vs == {"pass"}:
        return "pass"
    if vs == {"fail"}:
        return "fail"
    return "inconclusive"


def write_json(path: str, obj) -> None:
    with open(path, "w") as fh:
        json.dump(obj, fh, indent=2, sort_keys=True, default=str)
        fh.write("\n")


def run(cache_db: str, skip_stability: bool) -> None:
    proto = load_protocol()
    manifest = verify_frozen()
    build_cache(cache_db, manifest)
    forbid_network()
    import backtester as bt_mod
    runner = Runner(proto)
    probe = bt_mod.Backtester(initial_capital=proto["capital_usd"], platform=proto["fee_platform"])
    cost = {"commission_pct": getattr(probe, "commission_pct", None),
            "slippage_pct": getattr(probe, "slippage_pct", None),
            "intrabar_resolution": proto["intrabar_resolution"],
            "capital_usd": proto["capital_usd"]}
    started = time.time()
    out = {"protocol_sha256": sha256_file(PROTOCOL_PATH), "manifest_sha256": sha256_file(MANIFEST_PATH),
           "source_commit": git_head(), "cost_model": cost, "groups": {}}
    cand = proto["candidate"]
    for group in proto["groups"]:
        g = {"expected_interval_ms": proto["groups"][group]["expected_interval_ms"]}
        training = {}
        selections = {}
        for name in [cand] + proto["comparators"]:
            grid = proto["grids"][name]
            rows = [runner.evaluate(name, p, group, proto["training_window"]) for p in grid_combos(grid)]
            training[name] = rows
            fallback = proto["seed_params"] if name == cand else {
                k: runner.reg.STRATEGY_REGISTRY[name]["default_params"][k] for k in grid}
            selections[name] = select(rows, proto["selection_rule"], list(grid), fallback)
            print(f"[{group}] trained {name}: {selections[name]}", flush=True)
        g["training"] = training
        g["selections"] = selections
        with open(os.path.join(HERE, f"selected_{group}.json"), "w") as fh:
            json.dump(candidate_spec(proto, cand, runner.params_for(cand, group, selections[cand]["params"])),
                      fh, indent=2, sort_keys=True)
            fh.write("\n")
        confirmation = {}
        comps = {c: {} for c in proto["comparators"]}
        for window in proto["confirmation_windows"]:
            confirmation[window] = runner.evaluate(cand, selections[cand]["params"], group, window)
            for c in proto["comparators"]:
                comps[c][window] = runner.evaluate(c, selections[c]["params"], group, window)
            print(f"[{group}] confirmed {window}: {confirmation[window]['verdict']}", flush=True)
        g["confirmation"] = confirmation
        g["comparators"] = comps
        g["verdict"] = group_verdict(proto, confirmation, comps)
        out["groups"][group] = g
    out["verdict"] = overall_verdict({k: v["verdict"] for k, v in out["groups"].items()})
    if not skip_stability:
        for group in proto["groups"]:
            sweep = {}
            for window in proto["confirmation_windows"]:
                rows = [runner.evaluate(cand, p, group, window) for p in grid_combos(proto["grids"][cand])]
                sweep[window] = [{"params": r["params"], "verdict": r["verdict"], "mean_sharpe": r["mean_sharpe"],
                                  "mean_ddadj": r["mean_ddadj"],
                                  "mean_return_pct": round(statistics.mean(
                                      l["return_pct"] for l in r["legs"].values() if l), 4),
                                  "positions": sum(l["positions"] for l in r["legs"].values() if l)}
                                 for r in rows]
            out["groups"][group]["stability"] = sweep
    out["frames"] = {f"{s} {tf} {w}": v for (s, tf, w), v in sorted(runner.frames.items())}
    out["runtime_seconds"] = round(time.time() - started, 1)
    write_json(RESULTS_PATH, out)
    print(f"verdict: {out['verdict']}; wrote {RESULTS_PATH}")


PARITY_SPANS = {"1h": ("2026-07-01", "2026-10-01"), "4h": ("2025-10-01", "2026-10-01")}
PARITY_PATH = os.path.join(HERE, "parity_results.json")


def parity(cache_db: str) -> None:
    proto = load_protocol()
    manifest = verify_frozen()
    build_cache(cache_db, manifest)
    forbid_network()
    import pandas as pd
    from data_fetcher import load_cached_data
    from parity_diff import compute_parity_frame, config_from_live_config, summarize
    out = {"manifest_sha256": sha256_file(MANIFEST_PATH), "source_commit": git_head(), "runs": []}
    for tf in proto["groups"]:
        path = os.path.join(HERE, f"paper_parity_{tf}.json")
        with open(path) as fh:
            sid = json.load(fh)["strategies"][0]["id"]
        start, end = PARITY_SPANS[tf]
        df = load_cached_data("BTC/USDT", tf, start_date=start, end_date=end)
        df = df[df.index < pd.Timestamp(end)]
        param_sets = {"seed": None}
        sel = os.path.join(HERE, f"selected_{tf}.json")
        if os.path.exists(sel):
            with open(sel) as fh:
                param_sets["selected"] = json.load(fh)["params"]
        for label, override in param_sets.items():
            for batched in (False, True):
                for window in (200, None):
                    cfg = config_from_live_config(path, sid)
                    cfg.batched = batched
                    cfg.funding_mode = proto["funding_mode"]
                    if override is not None:
                        cfg.params = dict(override)
                    t0 = time.time()
                    frame = compute_parity_frame(df, cfg=cfg, window=window)
                    summ = summarize(frame)
                    row = {
                        "config": os.path.basename(path), "strategy_id": sid, "params_label": label,
                        "params": cfg.params, "close_refs": cfg.close_refs, "direction": cfg.direction,
                        "stop_kwargs": cfg.stop_kwargs, "batched": batched,
                        "window": 0 if window is None else window, "span": [start, end],
                        "candles": int(len(df)), "bars_compared": summ["bars_compared"],
                        "mismatches": summ["mismatches"], "batch_mismatches": summ.get("batch_mismatches"),
                        "decision_agreement": summ["decision_agreement"], "close_parity": summ["close_parity"],
                        "clean": summ["clean"],
                        "live_long_signals": int((frame["live_signal"] == 1).sum()),
                        "live_short_signals": int((frame["live_signal"] == -1).sum()),
                        "seconds": round(time.time() - t0, 1),
                    }
                    out["runs"].append(row)
                    print(json.dumps({k: row[k] for k in ("config", "params_label", "batched", "window",
                                                          "bars_compared", "mismatches", "batch_mismatches",
                                                          "clean")}), flush=True)
    out["clean"] = all(r["clean"] and r["bars_compared"] > 0 and not r["batch_mismatches"]
                       for r in out["runs"])
    write_json(PARITY_PATH, out)
    print(f"parity clean: {out['clean']}; wrote {PARITY_PATH}")


def main(argv=None) -> int:
    p = argparse.ArgumentParser(description="#1661 Chande Momentum Oscillator study (suggest-only)")
    sub = p.add_subparsers(dest="cmd", required=True)
    a = sub.add_parser("acquire", help="fetch candles and funding into an empty cache and freeze data/")
    a.add_argument("--cache-db", required=True)
    a.add_argument("--resume", action="store_true")
    r = sub.add_parser("run", help="rebuild a fresh cache from data/ and run the frozen protocol offline")
    r.add_argument("--cache-db", required=True)
    r.add_argument("--skip-stability", action="store_true")
    q = sub.add_parser("parity", help="decision parity of the paper configurations on frozen BTC candles")
    q.add_argument("--cache-db", required=True)
    sub.add_parser("render", help="re-render REPORT.md from results.json")
    args = p.parse_args(argv)
    if args.cmd == "acquire":
        acquire(args.cache_db, args.resume)
    elif args.cmd == "run":
        run(args.cache_db, args.skip_stability)
    elif args.cmd == "parity":
        parity(args.cache_db)
    if args.cmd in ("run", "parity", "render") and os.path.exists(RESULTS_PATH):
        from render_report import render
        render(load_protocol(), RESULTS_PATH, MANIFEST_PATH, REPORT_PATH)
    return 0


if __name__ == "__main__":
    sys.exit(main())

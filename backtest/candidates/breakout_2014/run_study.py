#!/usr/bin/env python3

import argparse
import copy
import csv
import gzip
import hashlib
import io
import json
import multiprocessing
import os
import shutil
import socket
import sqlite3
import statistics
import subprocess
import sys
import time
from collections.abc import MutableMapping

HERE = os.path.dirname(os.path.abspath(__file__))
BACKTEST = os.path.abspath(os.path.join(HERE, "..", ".."))
REPO = os.path.abspath(os.path.join(BACKTEST, ".."))
DATA = os.path.join(HERE, "data")
PROXY_DIR = os.path.join(DATA, "proxy")
ARMS_DIR = os.path.join(HERE, "arms")
DEPLOYED_PATH = os.path.join(HERE, "deployed_breakout_sanitized.json")
PROTOCOL_PATH = os.path.join(HERE, "protocol.json")
LOCK_PATH = os.path.join(HERE, "protocol_lock.json")
STUDY_MANIFEST_PATH = os.path.join(HERE, "study_manifest.json")
COVERAGE_PATH = os.path.join(HERE, "coverage_report.json")
CACHE_PARITY_PATH = os.path.join(HERE, "cache_parity.json")
TIMING_PATH = os.path.join(HERE, "timing.json")
SELECTION_PATH = os.path.join(HERE, "selection.json")
RESULTS_PATH = os.path.join(HERE, "results.json")
RUN_LOG_PATH = os.path.join(HERE, "run_log.json")
FIDELITY_PATH = os.path.join(HERE, "fidelity.json")
VERDICT_PATH = os.path.join(HERE, "verdict.json")
SUGGESTED_PATH = os.path.join(HERE, "suggested_paper_config.json")
REPORT_PATH = os.path.join(HERE, "REPORT.md")
MANIFEST_FILES = {
    ("venue", "30m"): "venue_manifest_30m.json",
    ("venue", "1h"): "venue_manifest_1h.json",
    ("bridge", "30m"): "bridge_manifest_30m.json",
    ("bridge", "1h"): "bridge_manifest_1h.json",
}
CACHE_ENV = "GO_TRADER_OHLCV_CACHE_DB"
DEFAULT_DB = os.path.join(REPO, "shared_tools", "trading_bot.db")
HOUR_MS = 3_600_000
TF_MS = {"30m": 1_800_000, "1h": 3_600_000}
ENGINE_FILES = (
    "backtest/backtester.py",
    "backtest/eval_windows.py",
    "backtest/regime_label_columns.py",
    "backtest/run_backtest.py",
    "backtest/tune_live.py",
    "backtest/offline_manifest.py",
    "shared_tools/regime.py",
    "shared_tools/funding_fetcher.py",
    "shared_strategies/open/registry.py",
    "shared_strategies/close/trailing_tp_ratchet.py",
)
LABELLER_FILES = (
    "backtest/regime_label_columns.py",
    "shared_tools/regime.py",
    "shared_tools/atr.py",
    "shared_strategies/open/adx_trend.py",
    "shared_strategies/open/indicators_core.py",
)
VALIDATOR_FILES = (
    "backtest/regime_bounded_window_validate.py",
    "backtest/regime_calibrate.py",
    "backtest/regime_diagnostics.py",
    "backtest/regime_hmm.py",
    "backtest/regime_stats.py",
)
for _p in (os.path.join(REPO, "shared_tools"), BACKTEST):
    if _p not in sys.path:
        sys.path.insert(0, _p)


def sha256_bytes(data: bytes) -> str:
    return hashlib.sha256(data).hexdigest()


def sha256_file(path: str) -> str:
    with open(path, "rb") as fh:
        return sha256_bytes(fh.read())


def load_json(path: str):
    with open(path) as fh:
        return json.load(fh)


def dumps(obj) -> str:
    return json.dumps(obj, indent=2, sort_keys=True, default=str) + "\n"


def write_json(path: str, obj) -> None:
    tmp = path + ".tmp"
    with open(tmp, "w") as fh:
        fh.write(dumps(obj))
    os.replace(tmp, path)


def write_gz_csv(path: str, header: list, rows: list) -> None:
    buf = io.StringIO()
    w = csv.writer(buf, lineterminator="\n")
    w.writerow(header)
    w.writerows(rows)
    with open(path, "wb") as fh:
        with gzip.GzipFile(filename="", mode="wb", fileobj=fh, mtime=0) as gz:
            gz.write(buf.getvalue().encode())


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


def to_ms(day: str) -> int:
    import pandas as pd
    return int(pd.Timestamp(day, tz="UTC").timestamp() * 1000)


def ms_str(ms) -> str:
    import pandas as pd
    return str(pd.Timestamp(int(ms), unit="ms"))


def strategy_id(coin: str, tf: str) -> str:
    return f"breakout-{coin.lower()}-{tf}"


def proxy_symbol(coin: str) -> str:
    return f"{coin}/USDT"


def candle_file(coin: str, tf: str) -> str:
    return f"proxy/{coin}_{tf}_candles.csv.gz"


def funding_file(coin: str) -> str:
    return f"proxy/{coin}_funding.csv.gz"


def protocol() -> dict:
    return load_json(PROTOCOL_PATH)


def protocol_sha256() -> str:
    return sha256_file(PROTOCOL_PATH)


def engine_sha256() -> dict:
    return {p: sha256_file(os.path.join(REPO, p)) for p in ENGINE_FILES}


def files_digest(paths: tuple) -> str:
    return sha256_bytes(json.dumps({p: sha256_file(os.path.join(REPO, p)) for p in paths},
                                   sort_keys=True).encode())


def labeller_sha256() -> str:
    return files_digest(LABELLER_FILES)


def label_cache_root(work_dir: str, name: str = "labels") -> str:
    return os.path.join(work_dir, name, labeller_sha256())


def checkpoint_fresh(path: str, fingerprint: str):
    if not os.path.exists(path):
        return None
    held = load_json(path)
    return held if held.get("fingerprint") == fingerprint else None


def refuse_default_db(path: str) -> str:
    path = os.path.abspath(path)
    if os.path.realpath(path) == os.path.realpath(DEFAULT_DB):
        raise SystemExit(f"refusing the default cache database {DEFAULT_DB}")
    if os.path.exists(path):
        raise SystemExit(f"refusing to reuse an existing cache {path}; pass a new path")
    return path


def set_cache(path: str) -> None:
    os.environ[CACHE_ENV] = os.path.abspath(path)


def refuse_network(*_a, **_k):
    raise RuntimeError("frozen study run: network access is blocked")


def block_network() -> None:
    socket.socket.connect = refuse_network
    socket.socket.connect_ex = refuse_network
    socket.create_connection = refuse_network
    socket.getaddrinfo = refuse_network
    import data_fetcher
    import funding_fetcher
    data_fetcher.fetch_full_history = refuse_network
    data_fetcher.fetch_ohlcv = refuse_network
    funding_fetcher._hl_adapter = refuse_network


def arm_configs(proto: dict) -> dict:
    base = load_json(DEPLOYED_PATH)
    out = {}
    for name, rule in sorted(proto["arm_configs"].items()):
        cfg = copy.deepcopy(base)
        for key, spec in sorted((rule.get("add_regime_windows") or {}).items()):
            cfg["regime"]["windows"][key] = copy.deepcopy(spec)
        for sc in cfg["strategies"]:
            for key, value in sorted((rule.get("strategy_overrides") or {}).items()):
                sc[key] = copy.deepcopy(value)
        out[name] = cfg
    return out


def arm_config_path(proto: dict, arm: str) -> str:
    return os.path.join(HERE, proto["arms"][arm]["config"])


def build_arm_candidate(proto: dict, arm: str, coin: str, tf: str) -> dict:
    import eval_windows as ew
    from run_backtest import load_strategy_config
    from tune_live import build_candidate
    res = load_strategy_config(arm_config_path(proto, arm), strategy_id(coin, tf), inject_user_defaults=True)
    open_ref = res["open_strategy"]
    cand = build_candidate(open_ref["name"], open_ref.get("params") or {}, res)
    if proto["arms"][arm]["policy"]:
        cand["regime_directional_policy"] = copy.deepcopy(res["regime_directional_policy"])
    ew.validate_candidate(cand)
    return cand


def first_study_leg_kwargs(proto: dict):
    import importlib.util
    path = os.path.join(REPO, proto["audit_rerun"]["driver"])
    spec = importlib.util.spec_from_file_location("breakout_1165_driver_common", path)
    mod = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(mod)
    return mod.candidate_leg_kwargs


def reference_candidate(proto: dict, arm: str) -> dict:
    import eval_windows as ew
    cand = load_json(os.path.join(REPO, proto["reference_arms"][arm]))
    ew.validate_candidate(cand)
    return cand


def build_arms(check: bool) -> None:
    proto = protocol()
    want = {name: dumps(cfg) for name, cfg in arm_configs(proto).items()}
    os.makedirs(ARMS_DIR, exist_ok=True)
    bad = []
    for name, text in sorted(want.items()):
        path = os.path.join(ARMS_DIR, f"{name}.json")
        if check:
            if not os.path.exists(path) or open(path).read() != text:
                bad.append(name)
        else:
            with open(path, "w") as fh:
                fh.write(text)
    if bad:
        raise SystemExit(f"arms differ from the pure build of the deployed copy: {bad}")
    import numpy as np
    import pandas as pd
    from registry_loader import load_registry
    from run_backtest import load_strategy_config
    from regime_label_columns import regime_label_report
    reg = load_registry(proto["registry"])
    n = 400
    idx = pd.date_range("2025-01-01", periods=n, freq="1h")
    c = 100 * np.exp(np.cumsum(np.random.default_rng(0).normal(0, 0.01, n)))
    probe = reg.apply_strategy("breakout", pd.DataFrame({"open": c, "high": c * 1.01, "low": c * 0.99, "close": c,
                                                        "volume": 1.0}, index=idx), {})
    if "atr" not in probe.columns:
        raise SystemExit("breakout no longer writes its own atr column; the atr_method disclosure is stale")
    report = {}
    for arm in sorted(proto["arms"]):
        rows = {}
        for coin in proto["coins"]:
            for tf in proto["intervals"]:
                res = load_strategy_config(arm_config_path(proto, arm), strategy_id(coin, tf),
                                           inject_user_defaults=True)
                if res["direction"] != "long" or res["regime_directional_certified"]:
                    raise SystemExit(f"{arm} {coin} {tf}: unexpected direction or certification")
                cand = build_arm_candidate(proto, arm, coin, tf)
                plan = cand["regime_label_windows"]
                rows[f"{coin} {tf}"] = {
                    "direction": cand["direction"], "close": [r["name"] for r in cand["close_strategies"]],
                    "stop_owner": sorted(k for k in cand["stop_context"] if k.endswith("_regime")),
                    "allowed_regimes": cand.get("allowed_regimes"),
                    "policy": bool(cand.get("regime_directional_policy")),
                    "label_windows": regime_label_report(plan)["windows"], "label_limit": plan["limit"],
                    "atr_method": res["atr_method"],
                }
        report[arm] = rows
        first = next(iter(rows.values()))
        print(f"{arm}: gate {first['label_windows']['gate']['window']} atr {first['label_windows']['atr']['window']} "
              f"allowed {first['allowed_regimes']} policy {first['policy']} limit {first['label_limit']}")
    print("arms ok" if check else "arms written")


def study_windows(proto: dict) -> dict:
    return {k: tuple(v) for k, v in proto["proxy"]["windows"].items()}


def fetch_candles(ex, symbol: str, tf: str, start_ms: int, end_ms: int) -> list:
    import ccxt
    step = TF_MS[tf]
    rows, cursor = {}, start_ms
    while cursor < end_ms:
        for attempt in range(8):
            try:
                batch = ex.fetch_ohlcv(symbol, tf, since=cursor, limit=1000)
                break
            except (ccxt.NetworkError, ccxt.RateLimitExceeded) as exc:
                print(f"{symbol} {tf} {cursor}: {str(exc)[:120]}; retry {attempt + 1}", flush=True)
                time.sleep(10 * (attempt + 1))
        else:
            raise SystemExit(f"{symbol} {tf} candles at {cursor} could not be fetched")
        if not batch:
            break
        for t, o, h, lo, c, v in batch:
            t = int(t)
            if start_ms <= t < end_ms and t + step <= end_ms:
                rows[t] = [t, repr(float(o)), repr(float(h)), repr(float(lo)), repr(float(c)), repr(float(v))]
        last = int(batch[-1][0])
        if last + step <= cursor:
            break
        cursor = last + step
        time.sleep(ex.rateLimit / 1000)
    return [rows[t] for t in sorted(rows)]


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


def acquire_proxy(cache_db: str) -> None:
    proto = protocol()
    if os.environ.get("HYPERLIQUID_SECRET_KEY"):
        raise SystemExit("refusing to acquire research data with HYPERLIQUID_SECRET_KEY set")
    cache_db = refuse_default_db(cache_db)
    if os.path.exists(STUDY_MANIFEST_PATH) or (os.path.isdir(PROXY_DIR) and os.listdir(PROXY_DIR)):
        raise SystemExit("frozen proxy inputs already exist; acquisition runs once")
    set_cache(cache_db)
    import storage
    from data_fetcher import get_exchange
    from funding_fetcher import load_cached_funding
    import pandas as pd
    storage.init_db(cache_db)
    acq = proto["proxy"]["acquisition"]
    start_ms, end_ms = to_ms(acq["start"]), to_ms(proto["proxy"]["pinned_end"])
    ex = get_exchange(acq["exchange"])
    markets = ex.load_markets()
    probe = {proxy_symbol(c): proxy_symbol(c) in markets for c in proto["coins"]}
    print(f"market probe: {probe}", flush=True)
    os.makedirs(PROXY_DIR, exist_ok=True)
    files = {}
    coins = [c for c in proto["coins"] if probe[proxy_symbol(c)]]
    for coin in coins:
        for tf in proto["intervals"]:
            rows = fetch_candles(ex, proxy_symbol(coin), tf, start_ms, end_ms)
            if not rows:
                raise SystemExit(f"{coin} {tf}: no candles")
            path = os.path.join(DATA, candle_file(coin, tf))
            write_gz_csv(path, ["timestamp", "open", "high", "low", "close", "volume"], rows)
            ts = [r[0] for r in rows]
            gaps = [[a, b, (b - a) // TF_MS[tf] - 1] for a, b in zip(ts, ts[1:]) if b - a != TF_MS[tf]]
            files[candle_file(coin, tf)] = {
                "kind": "candles", "exchange": acq["exchange"], "symbol": proxy_symbol(coin), "timeframe": tf,
                "rows": len(rows), "first_ms": ts[0], "last_ms": ts[-1], "first_bar": ms_str(ts[0]),
                "last_bar": ms_str(ts[-1]), "gaps": gaps, "missing_bars": sum(g[2] for g in gaps),
                "sha256": sha256_file(path),
            }
            print(f"{coin} {tf}: {len(rows)} candles {ms_str(ts[0])} .. {ms_str(ts[-1])}, {len(gaps)} gaps",
                  flush=True)
    for coin in coins:
        cursor, chunk = start_ms, 30 * 24 * HOUR_MS
        while cursor < end_ms:
            stop = min(cursor + chunk, end_ms)
            for attempt in range(8):
                box = {}
                load_cached_funding(coin, pd.Timestamp(cursor, unit="ms", tz="UTC"),
                                    end_date=pd.Timestamp(stop, unit="ms", tz="UTC"), db_path=cache_db,
                                    source_box=box)
                if not box.get("fetch_error"):
                    break
                print(f"{coin} funding {cursor}..{stop}: {box['fetch_error'][:120]}; retry {attempt + 1}", flush=True)
                time.sleep(15 * (attempt + 1))
            else:
                raise SystemExit(f"{coin} funding {cursor}..{stop} could not be fetched")
            cursor = stop
        missing = missing_funding_hours(cache_db, coin, start_ms, end_ms)
        first_rate = storage.load_funding_first_ts("hyperliquid", coin, db_path=cache_db)
        late = [h for h in missing if first_rate is None or h + HOUR_MS > first_rate]
        if late:
            raise SystemExit(f"{coin} funding has {len(late)} hours after the venue's first rate with neither a "
                             f"rate nor a venue-absent mark, first {late[:3]}")
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
        files[funding_file(coin)] = {
            "kind": "funding", "exchange": "hyperliquid", "coin": coin, "rows": len(rates),
            "first_ms": int(rates[0][0]) if rates else None, "last_ms": int(rates[-1][0]) if rates else None,
            "coverage": [[start_ms, end_ms]],
            "stored_coverage_intervals": [[int(s), int(e)] for s, e in coverage],
            "pre_venue_hours": len(missing), "first_venue_rate_ms": first_rate,
            "first_venue_rate": ms_str(first_rate) if first_rate else None,
            "venue_absent_hours": [[int(t), int(o)] for t, o in gaps], "sha256": sha256_file(path),
        }
        print(f"{coin} funding: {len(rates)} rates, first {files[funding_file(coin)]['first_venue_rate']}, "
              f"{len(gaps)} venue-absent hours", flush=True)
    write_json(STUDY_MANIFEST_PATH, {
        "study": proto["study"], "acquired_at_utc": time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime()),
        "source_commit": git_head(), "acquisition_start": acq["start"], "pinned_end": proto["proxy"]["pinned_end"],
        "market_probe": {"exchange": acq["exchange"], "symbols": probe},
        "candles": "ccxt binanceus fetch_ohlcv in 1000-bar pages from acquisition_start; closed bars with opening "
                   "time >= acquisition_start and < pinned_end; a failed page aborts the acquisition",
        "funding": "Hyperliquid fundingHistory through shared_tools/funding_fetcher.load_cached_funding in 30-day "
                   "chunks into a new throwaway database; every hour after the venue's first rate has a rate or a "
                   "venue-absent mark; the pre-venue hours stay unpriced",
        "files": dict(sorted(files.items())),
    })
    print(f"wrote {len(files)} frozen files and {STUDY_MANIFEST_PATH}")


def load_manifests(verify: bool = True) -> dict:
    import offline_manifest as om
    out = {}
    for key, name in sorted(MANIFEST_FILES.items()):
        path = os.path.join(HERE, name)
        if os.path.exists(path):
            out[key] = om.load_manifest(path, verify=verify)
    return out


def verify_frozen() -> dict:
    manifest = load_json(STUDY_MANIFEST_PATH)
    for name, meta in sorted(manifest["files"].items()):
        got = sha256_file(os.path.join(DATA, name))
        if got != meta["sha256"]:
            raise SystemExit(f"frozen input data/{name} sha256 {got} != manifest {meta['sha256']}")
    manifests = load_manifests(verify=True)
    if len(manifests) != len(MANIFEST_FILES):
        raise SystemExit(f"expected {len(MANIFEST_FILES)} offline manifests, found {sorted(manifests)}")
    return {"proxy": manifest, "manifests": manifests}


def build_cache(cache_db: str, manifest: dict) -> None:
    set_cache(cache_db)
    import pandas as pd
    import storage
    storage.init_db(cache_db)
    for name, meta in sorted(manifest["files"].items()):
        rows = read_gz_csv(os.path.join(DATA, name))
        if meta["kind"] == "candles":
            df = pd.DataFrame(rows, columns=["timestamp", "open", "high", "low", "close", "volume"])
            df = df.astype({"timestamp": "int64", "open": float, "high": float, "low": float, "close": float,
                            "volume": float})
            storage.store_ohlcv(df, meta["exchange"], meta["symbol"], meta["timeframe"], db_path=cache_db)
        else:
            records = [{"time": int(t), "rate": float(r)} for t, r in rows]
            absent = [h for h, _ in meta["venue_absent_hours"]]
            observed = max((o for _, o in meta["venue_absent_hours"]), default=None)
            storage.store_funding_fetch(meta["exchange"], meta["coin"], records, absent,
                                        observed_through_ms=observed, db_path=cache_db)
            for s, e in meta["coverage"]:
                storage.store_funding_coverage(meta["exchange"], meta["coin"], s, e, db_path=cache_db)
    conn = sqlite3.connect(cache_db)
    try:
        conn.execute("PRAGMA wal_checkpoint(TRUNCATE)")
    finally:
        conn.close()


def candle_frame(name: str):
    import pandas as pd
    rows = read_gz_csv(os.path.join(DATA, name))
    df = pd.DataFrame(rows, columns=["timestamp", "open", "high", "low", "close", "volume"])
    df = df.astype({"timestamp": "int64", "open": float, "high": float, "low": float, "close": float,
                    "volume": float})
    df.index = pd.to_datetime(df["timestamp"].to_numpy(), unit="ms")
    df.index.name = "datetime"
    return df


def funding_hours(name: str) -> set:
    return {int(t) // HOUR_MS * HOUR_MS for t, _ in read_gz_csv(os.path.join(DATA, name))}


def prefix_ok(ts: list, start_ms: int, tf: str, warmup: int) -> dict:
    import bisect
    step = TF_MS[tf]
    i = bisect.bisect_left(ts, start_ms)
    want = [start_ms - step * k for k in range(warmup, 0, -1)]
    got = ts[max(0, i - warmup):i]
    ok = got == want
    missing = sorted(set(want) - set(got))
    return {"ok": ok, "rows_before_start": i, "missing": len(missing),
            "first_missing": ms_str(missing[0]) if missing else None}


def coverage() -> None:
    proto = protocol()
    frozen = verify_frozen()
    block_network()
    import offline_manifest as om
    warm = proto["warmup_bars"]
    windows = study_windows(proto)
    pinned_end = to_ms(proto["proxy"]["pinned_end"])
    files = frozen["proxy"]["files"]
    probe = frozen["proxy"]["market_probe"]["symbols"]
    rows = []
    for coin in proto["coins"]:
        for tf in proto["intervals"]:
            row = {"dataset": f"{coin} {tf}", "group": "proxy", "coin": coin, "interval": tf, "windows": {}}
            if candle_file(coin, tf) not in files:
                row.update({"status": "excluded",
                            "reason": f"no proxy market: {proxy_symbol(coin)} absent from the "
                                      f"{frozen['proxy']['market_probe']['exchange']} market list "
                                      f"(probe {probe.get(proxy_symbol(coin))})"})
                rows.append(row)
                continue
            ts = [int(r[0]) for r in read_gz_csv(os.path.join(DATA, candle_file(coin, tf)))]
            tsset = set(ts)
            priced = funding_hours(funding_file(coin))
            absent = {h for h, _ in files[funding_file(coin)]["venue_absent_hours"]}
            reasons = []
            for w, (start, end) in sorted(windows.items()):
                s, e = to_ms(start), to_ms(end) if end else pinned_end
                pre = prefix_ok(ts, s, tf, warm)
                expected = list(range(s, e, TF_MS[tf]))
                present = sum(1 for t in expected if t in tsset)
                unpriced = [h for h in range(s, e, HOUR_MS) if h not in priced and h not in absent]
                item = {"start": start, "end": end, "warmup": pre, "expected_bars": len(expected),
                        "present_bars": present, "missing_bars": len(expected) - present,
                        "unpriced_funding_hours": len(unpriced),
                        "first_unpriced_hour": ms_str(unpriced[0]) if unpriced else None,
                        "first_bar": ms_str(min(t for t in expected if t in tsset)) if present else None,
                        "last_bar": ms_str(max(t for t in expected if t in tsset)) if present else None}
                why = []
                if not pre["ok"]:
                    why.append(f"warm-up prefix lacks {warm} consecutive bars ({pre['missing']} missing, first "
                               f"{pre['first_missing']})")
                if unpriced:
                    why.append(f"{len(unpriced)} unpriced funding hours, first {item['first_unpriced_hour']}")
                if present == 0:
                    why.append("no candles in the window")
                item["status"] = "excluded" if why else "included"
                item["reason"] = "; ".join(why) if why else "complete warm-up prefix and priced funding"
                row["windows"][w] = item
                if why and w in proto["proxy"]["dataset_windows"]:
                    reasons.append(f"{w}: {item['reason']}")
            row["status"] = "excluded" if reasons else "included"
            row["reason"] = "; ".join(reasons) if reasons else (
                "warm-up prefix and funding complete on " + ", ".join(proto["proxy"]["dataset_windows"]))
            rows.append(row)
    manifests = frozen["manifests"]
    for coin in proto["coins"]:
        for tf in proto["intervals"]:
            m = manifests[("venue", tf)]
            row = {"dataset": f"{coin} {tf}", "group": "venue", "coin": coin, "interval": tf, "windows": {}}
            ds = next((d for d in m["datasets"] if d["coin"] == coin), None)
            if ds is None:
                row.update({"status": "excluded", "reason": "no venue dataset in the manifest"})
                rows.append(row)
                continue
            funding = om.load_funding(ds)
            reasons = []
            for w, win in sorted(m["windows"].items()):
                try:
                    _, _, cov = om.window_frame(m, ds, w)
                except om.ManifestError as exc:
                    row["windows"][w] = {"status": "excluded", "reason": str(exc)}
                    reasons.append(f"{w}: {exc}")
                    continue
                fcov = om.funding_coverage(funding, win["start_ms"], win["end_ms"])
                why = []
                if not cov["complete"]:
                    why.append(f"{cov['missing_bars']} missing candles")
                if cov["warmup_missing_bars"]:
                    why.append(f"{cov['warmup_missing_bars']} missing warm-up bars")
                if not fcov.get("complete"):
                    why.append(f"funding incomplete ({fcov.get('missing_hours')} missing hours)")
                row["windows"][w] = {"start": win["start"], "end": win["end"], "candles": cov, "funding": fcov,
                                     "status": "excluded" if why else "included",
                                     "reason": "; ".join(why) if why else "complete candles, warm-up and funding"}
                if why:
                    reasons.append(f"{w}: {'; '.join(why)}")
            row["status"] = "excluded" if reasons else "included"
            row["reason"] = "; ".join(reasons) if reasons else "complete candles, warm-up and funding on every window"
            bm = manifests[("bridge", tf)]
            bds = next((d for d in bm["datasets"] if d["coin"] == coin), None)
            row["bridge"] = None
            if bds is not None:
                bridge = {}
                for w in sorted(bm["windows"]):
                    try:
                        _, _, bcov = om.window_frame(bm, bds, w)
                        ok = not bcov["warmup_missing_bars"]
                        bridge[w] = {"candles": bcov, "status": "included" if ok else "excluded",
                                     "reason": (f"warm-up complete; {bcov['missing_bars']} missing proxy candles in "
                                                "the window are scored as they are" if ok else
                                                f"{bcov['warmup_missing_bars']} missing warm-up bars")}
                    except om.ManifestError as exc:
                        bridge[w] = {"status": "excluded", "reason": str(exc)}
                row["bridge"] = bridge
            rows.append(row)
    out = {"study": proto["study"], "protocol_sha256": protocol_sha256(),
           "manifest_sha256": sha256_file(STUDY_MANIFEST_PATH),
           "network_blocked": True, "warmup_bars": warm, "rows": rows,
           "included": {"proxy": sorted(r["dataset"] for r in rows if r["group"] == "proxy" and
                                        r["status"] == "included"),
                        "venue": sorted(r["dataset"] for r in rows if r["group"] == "venue" and
                                        r["status"] == "included")}}
    write_json(COVERAGE_PATH, out)
    for r in rows:
        print(f"{r['group']:<6} {r['dataset']:<9} {r['status']:<9} {r['reason']}")
    print(f"wrote {COVERAGE_PATH} ({len(rows)} rows)")


def lock() -> None:
    sha = protocol_sha256()
    if os.path.exists(LOCK_PATH):
        held = load_json(LOCK_PATH)
        if held["protocol_sha256"] != sha:
            raise SystemExit(f"protocol.json changed after the lock ({held['protocol_sha256']} -> {sha})")
        print(f"protocol already locked at {held['locked_at_utc']}: {sha}")
        return
    if any(os.path.exists(p) for p in (SELECTION_PATH, RESULTS_PATH)):
        raise SystemExit("scores exist; the protocol must be locked before any score is written")
    write_json(LOCK_PATH, {"protocol_sha256": sha, "locked_at_utc": time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime()),
                           "source_commit": git_head(), "coverage_sha256": sha256_file(COVERAGE_PATH),
                           "note": "written by run_study.py lock before any window was scored"})
    print(f"locked protocol.json {sha}")


def require_lock() -> dict:
    if not os.path.exists(LOCK_PATH):
        raise SystemExit("protocol_lock.json is missing; run `run_study.py lock` first")
    held = load_json(LOCK_PATH)
    if held["protocol_sha256"] != protocol_sha256():
        raise SystemExit(f"protocol.json sha256 {protocol_sha256()} differs from the lock {held['protocol_sha256']}")
    if held["coverage_sha256"] != sha256_file(COVERAGE_PATH):
        raise SystemExit("coverage_report.json changed after the lock")
    return held


class DiskLabelCache(MutableMapping):
    def __init__(self, root: str):
        self.root = root
        self.memo = {}
        self.hits = 0
        self.misses = 0
        os.makedirs(root, exist_ok=True)

    def _path(self, key: str) -> str:
        return os.path.join(self.root, key[:2], key + ".json.gz")

    def __contains__(self, key) -> bool:
        found = key in self.memo or os.path.exists(self._path(key))
        if found:
            self.hits += 1
        else:
            self.misses += 1
        return found

    def __getitem__(self, key):
        if key not in self.memo:
            try:
                with gzip.open(self._path(key), "rt") as fh:
                    self.memo[key] = json.load(fh)
            except FileNotFoundError:
                raise KeyError(key) from None
        return self.memo[key]

    def __setitem__(self, key, value) -> None:
        path = self._path(key)
        os.makedirs(os.path.dirname(path), exist_ok=True)
        tmp = f"{path}.{os.getpid()}.tmp"
        with gzip.open(tmp, "wt") as fh:
            json.dump(value, fh)
        os.replace(tmp, path)
        self.memo[key] = value

    def __delitem__(self, key) -> None:
        self.memo.pop(key, None)
        os.remove(self._path(key))

    def __iter__(self):
        for sub in sorted(os.listdir(self.root)):
            for name in sorted(os.listdir(os.path.join(self.root, sub))):
                if name.endswith(".json.gz"):
                    yield name[:-8]

    def __len__(self) -> int:
        return sum(1 for _ in self)

    def trim(self) -> None:
        self.memo.clear()


_WORKER = {}


def worker_init(base_db: str, work_dir: str) -> None:
    path = os.path.join(work_dir, "db", f"worker_{os.getpid()}.db")
    os.makedirs(os.path.dirname(path), exist_ok=True)
    shutil.copyfile(base_db, path)
    set_cache(path)
    for p in (os.path.join(REPO, "shared_tools"), BACKTEST):
        if p not in sys.path:
            sys.path.insert(0, p)
    block_network()
    from registry_loader import load_registry
    _WORKER["proto"] = protocol()
    _WORKER["reg"] = load_registry(_WORKER["proto"]["registry"])
    _WORKER["labels"] = DiskLabelCache(label_cache_root(work_dir))
    _WORKER["manifests"] = load_manifests(verify=True)


def trim_leg(leg):
    if leg is None:
        return None
    out = {k: v for k, v in leg.items() if k != "trade_samples"}
    return out


def unit_id(unit: dict) -> str:
    parts = [unit["group"], unit["interval"], unit["coin"], unit["window"]]
    if unit.get("tag"):
        parts.append(unit["tag"])
    return "_".join(parts)


def unit_fingerprint(unit: dict, proto_sha: str, eng: dict, arms_sha: dict, labeller: str) -> str:
    return sha256_bytes(json.dumps({"unit": unit, "protocol": proto_sha, "engine": eng, "arms": arms_sha,
                                    "labeller": labeller}, sort_keys=True).encode())


def fidelity_fingerprint(unit: dict, proto_sha: str, eng: dict, arms_sha: dict, labeller: str,
                         validator: str) -> str:
    return sha256_bytes(json.dumps({"unit": unit, "protocol": proto_sha, "engine": eng, "arms": arms_sha,
                                    "labeller": labeller, "validator": validator}, sort_keys=True).encode())


def arms_sha256(proto: dict) -> dict:
    return {a: sha256_file(arm_config_path(proto, a)) for a in sorted(proto["arms"])}


def proxy_effective_range(symbol: str, tf: str, window: tuple, warm: int) -> dict:
    import pandas as pd
    from data_fetcher import load_cached_data
    from eval_windows import warmup_load_start
    start, end = window
    load_start = warmup_load_start(start, tf, warm) if warm else start
    df = load_cached_data(symbol, tf, start_date=load_start, end_date=end)
    df = df[df.index < pd.Timestamp(end)]
    scored = df[df.index >= pd.Timestamp(start)]
    return {"warmup_first_bar": str(df.index[0]), "scored_first_bar": str(scored.index[0]),
            "scored_last_bar": str(scored.index[-1]), "scored_bars": int(len(scored)),
            "frame_bars": int(len(df))}


def run_unit(unit: dict) -> dict:
    import eval_windows as ew
    proto, reg, labels = _WORKER["proto"], _WORKER["reg"], _WORKER["labels"]
    capital = proto["capital_usd"]
    warm = proto["warmup_bars"]
    t0 = time.time()
    labels.hits = labels.misses = 0
    out = {"unit": unit, "legs": {}, "incumbent_legs": {}, "incumbent_bars": {}, "range": None}
    coin, tf = unit["coin"], unit["interval"]
    if unit["group"] == "proxy":
        symbol = proxy_symbol(coin)
        window = tuple(unit["range"])
        funding = unit.get("funding_mode", proto["funding_mode"])
        if unit.get("audit"):
            candidate_leg_kwargs = first_study_leg_kwargs(proto)
            for arm in unit["arms"]:
                cand = reference_candidate(proto, arm)
                leg = ew.run_leg(reg, cand["name"], cand.get("params"), symbol, tf, window,
                                 funding_mode=funding, **candidate_leg_kwargs(cand))
                out["legs"][arm] = {"1": trim_leg(leg)}
            out["range"] = proxy_effective_range(symbol, tf, window, 0)
        else:
            if unit.get("incumbents"):
                inc = ew.compute_incumbent_legs(reg, [(symbol, tf)], window, capital, funding_mode=funding,
                                                warmup_bars=warm)
                out["incumbent_legs"]["1"] = {k: trim_leg(v) for k, v in inc[ew.dataset_key(symbol, tf)].items()}
                out["incumbent_bars"]["1"] = ew.incumbent_bars(inc)[ew.dataset_key(symbol, tf)]
            for arm in unit["arms"]:
                ref = arm in proto["reference_arms"]
                cand = reference_candidate(proto, arm) if ref else build_arm_candidate(proto, arm, coin, tf)
                leg = ew.run_candidate_leg(reg, cand, symbol, tf, window, capital, keep_trades=True,
                                           funding_mode=funding, warmup_bars=warm,
                                           regime_label_cache=None if ref else labels)
                out["legs"][arm] = {"1": trim_leg(leg)}
            out["range"] = proxy_effective_range(symbol, tf, window, warm)
    else:
        m = _WORKER["manifests"][(unit["group"], tf)]
        for cost in unit["costs"]:
            ctx = {"manifest": m, "window": unit["window"], "cost_multiplier": cost}
            ck = f"{cost:g}"
            if unit.get("incumbents"):
                inc = ew.compute_incumbent_legs(reg, [(coin, tf)], None, capital, manifest_ctx=ctx,
                                                funding_mode=proto["funding_mode"])
                out["incumbent_legs"][ck] = {k: trim_leg(v) for k, v in inc[ew.dataset_key(coin, tf)].items()}
                out["incumbent_bars"][ck] = ew.incumbent_bars(inc)[ew.dataset_key(coin, tf)]
            for arm in unit["arms"]:
                cand = build_arm_candidate(proto, arm, coin, tf)
                leg = ew.run_candidate_leg(reg, cand, coin, tf, None, capital, keep_trades=True, manifest_ctx=ctx,
                                           funding_mode=proto["funding_mode"], regime_label_cache=labels)
                out["legs"].setdefault(arm, {})[ck] = trim_leg(leg)
        first = next(iter(next(iter(out["legs"].values())).values()))
        cov = first["manifest"]["candle_coverage"]
        out["range"] = {"scored_first_bar": cov["first_bar"], "scored_last_bar": cov["last_bar"],
                        "scored_bars": cov["present_bars"], "warmup_bars": cov["warmup_bars"]}
    out["seconds"] = round(time.time() - t0, 1)
    out["label_cache"] = {"hits": labels.hits, "misses": labels.misses}
    labels.trim()
    return out


def run_unit_checkpointed(task):
    unit, path, fingerprint = task
    held = checkpoint_fresh(path, fingerprint)
    if held is not None:
        return {"unit": unit_id(unit), "status": "cached", "seconds": held.get("seconds")}
    res = run_unit(unit)
    res["fingerprint"] = fingerprint
    res["pid"] = os.getpid()
    res["finished_utc"] = time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime())
    write_json(path, res)
    return {"unit": unit_id(unit), "status": "scored", "seconds": res["seconds"]}


def proxy_datasets(cov: dict, window: str) -> list:
    out = []
    for r in cov["rows"]:
        if r["group"] == "proxy" and r["status"] == "included" and r["windows"][window]["status"] == "included":
            out.append((r["coin"], r["interval"]))
    return out


def venue_datasets(cov: dict, window: str, tf: str) -> list:
    out = []
    for r in cov["rows"]:
        if (r["group"] == "venue" and r["interval"] == tf and r["status"] == "included"
                and window in r["windows"] and r["windows"][window]["status"] == "included"):
            out.append(r["coin"])
    return out


def bridge_datasets(cov: dict, window: str, tf: str) -> list:
    out = []
    for r in cov["rows"]:
        if r["group"] == "venue" and r["interval"] == tf and r.get("bridge") and \
                r["bridge"].get(window, {}).get("status") == "included":
            out.append(r["coin"])
    return out


def config_arms(proto: dict) -> list:
    return sorted(proto["arms"])


def select_units(proto: dict, cov: dict) -> list:
    w = proto["proxy"]["selection_window"]
    rng = study_windows(proto)[w]
    return [{"group": "proxy", "interval": tf, "coin": coin, "window": w, "range": list(rng),
             "arms": config_arms(proto), "incumbents": True} for coin, tf in proxy_datasets(cov, w)]


def judge_units(proto: dict, cov: dict) -> list:
    units = []
    wins = study_windows(proto)
    refs = sorted(proto["reference_arms"])
    for w in proto["proxy"]["judged_windows"]:
        for coin, tf in proxy_datasets(cov, w):
            arms = config_arms(proto) + (refs if w in proto["reference_windows"] else [])
            units.append({"group": "proxy", "interval": tf, "coin": coin, "window": w, "range": list(wins[w]),
                          "arms": arms, "incumbents": True})
    sel = proto["proxy"]["selection_window"]
    if sel in proto["reference_windows"]:
        for coin, tf in proxy_datasets(cov, sel):
            units.append({"group": "proxy", "interval": tf, "coin": coin, "window": sel, "range": list(wins[sel]),
                          "arms": refs, "incumbents": False, "tag": "refs"})
    costs = sorted(proto["venue"]["cost_multipliers"].values())
    for tf in proto["intervals"]:
        for w in proto["venue"]["windows"][tf]:
            for coin in venue_datasets(cov, w, tf):
                units.append({"group": "venue", "interval": tf, "coin": coin, "window": w, "costs": costs,
                              "arms": config_arms(proto), "incumbents": True})
            for coin in bridge_datasets(cov, w, tf):
                units.append({"group": "bridge", "interval": tf, "coin": coin, "window": w, "costs": costs,
                              "arms": config_arms(proto), "incumbents": True})
    audit = proto["audit_rerun"]
    headline = load_json(os.path.join(REPO, audit["headline"]))
    for key in audit["datasets"]:
        symbol, tf = key.split(" ")
        last = headline["effective_range"][key]["last_bar"]
        import pandas as pd
        end = str(pd.Timestamp(last) + pd.Timedelta(milliseconds=TF_MS[tf]))
        units.append({"group": "proxy", "interval": tf, "coin": symbol.split("/")[0], "window": "audit",
                      "range": [audit["start"], end], "arms": sorted(audit["arms"]), "incumbents": False,
                      "audit": True, "funding_mode": audit["funding_mode"], "tag": "audit"})
    return units


def unit_weight(unit: dict) -> float:
    import pandas as pd
    if unit["group"] == "proxy":
        s, e = unit["range"]
        days = (pd.Timestamp(e) - pd.Timestamp(s)).days
    else:
        days = 100
    per = 48 if unit["interval"] == "30m" else 24
    return days * per * len(unit["arms"])


def run_units(units: list, phase: str, work_dir: str, base_db: str, workers: int) -> list:
    proto = protocol()
    proto_sha, eng, arms, lab = protocol_sha256(), engine_sha256(), arms_sha256(proto), labeller_sha256()
    ckdir = os.path.join(work_dir, "ckpt", phase)
    os.makedirs(ckdir, exist_ok=True)
    tasks = []
    for u in sorted(units, key=unit_weight, reverse=True):
        tasks.append((u, os.path.join(ckdir, unit_id(u) + ".json"),
                      unit_fingerprint(u, proto_sha, eng, arms, lab)))
    done = []
    ctx = multiprocessing.get_context("spawn")
    started = time.time()
    with ctx.Pool(processes=workers, initializer=worker_init, initargs=(base_db, work_dir)) as pool:
        for i, res in enumerate(pool.imap_unordered(run_unit_checkpointed, tasks), 1):
            done.append(res)
            print(f"[{phase}] {i}/{len(tasks)} {res['unit']} {res['status']} {res['seconds']}s "
                  f"(elapsed {round(time.time() - started)}s)", flush=True)
    return [load_json(t[1]) for t in tasks]


def prepare_offline(cache_db: str, work_dir: str) -> tuple:
    cache_db = refuse_default_db(cache_db)
    frozen = verify_frozen()
    build_cache(cache_db, frozen["proxy"])
    block_network()
    os.makedirs(work_dir, exist_ok=True)
    return frozen, cache_db


def network_probe() -> dict:
    out = {}
    from data_fetcher import load_cached_data
    try:
        load_cached_data("DOGE/USDT", "1h", start_date="2024-01-01", end_date="2024-02-01")
        out["unfrozen_symbol_load"] = "loaded (unexpected)"
    except RuntimeError as exc:
        out["unfrozen_symbol_load"] = f"refused: {exc}"
    try:
        socket.create_connection(("api.hyperliquid.xyz", 443), timeout=5)
        out["socket_connect"] = "connected (unexpected)"
    except RuntimeError as exc:
        out["socket_connect"] = f"refused: {exc}"
    if any("unexpected" in v for v in out.values()):
        raise SystemExit(f"network probe failed: {out}")
    return out


def position_clusters(positions: list) -> int:
    import pandas as pd
    spans = sorted((pd.Timestamp(p["entry_date"]), pd.Timestamp(p["exit_date"])) for p in positions)
    count, end = 0, None
    for s, e in spans:
        if end is None or s > end:
            count += 1
            end = e
        else:
            end = max(end, e)
    return count


def leg_funding(leg: dict) -> dict:
    block = leg.get("funding") or {}
    if not block and leg.get("manifest"):
        cov = leg["manifest"]["funding_coverage"]
        return {"source": "manifest", "complete": cov.get("complete"), "missing_hours": cov.get("missing_hours")}
    return {"source": block.get("source"), "complete": block.get("complete"), "mode": block.get("mode"),
            "unpriced_held_hours": block.get("unpriced_held_hours")}


def summarize(legs: dict, bars: dict, capital: float) -> dict:
    import eval_windows as ew
    present = {k: v for k, v in legs.items() if v is not None}
    score = ew.score_candidate(legs, bars)
    out = {"datasets": len(present), "m1_verdict": score.get("verdict"), "m1_bar_sharpe": score.get("mean_bar_sharpe"),
           "m1_bar_ddadj": score.get("mean_bar_ddadj")}
    if not present:
        out.update({"mean_return_pct": None, "mean_sharpe": None, "mean_ddadj": None, "worst_max_dd_pct": None,
                    "trades": 0, "positions": 0, "clusters": 0, "fee_drag_pct": None, "funding_drag_pct": None,
                    "funding_incomplete_legs": 0, "funding_fetched_legs": 0, "per_dataset": {}})
        return out
    per, positions = {}, []
    for ds, leg in sorted(present.items()):
        ex = leg.get("execution") or {}
        item = {"return_pct": leg["return_pct"], "sharpe": leg["sharpe"], "max_dd_pct": leg["max_dd_pct"],
                "ddadj": leg["ddadj"], "trades": leg["trades"], "bh_return_pct": leg.get("bh_return_pct"),
                "positions": ex.get("positions"), "long_positions": ex.get("long_positions"),
                "short_positions": ex.get("short_positions"), "fees_usd": ex.get("fees_usd"),
                "funding_pnl_usd": ex.get("funding_pnl_usd"), "net_pnl_usd": ex.get("net_pnl_usd"),
                "fee_drag_pct": round(100.0 * (ex.get("fees_usd") or 0.0) / capital, 4),
                "funding_drag_pct": round(-100.0 * (ex.get("funding_pnl_usd") or 0.0) / capital, 4),
                "liquidated": bool(leg.get("liquidated")), "funding_incomplete": bool(leg.get("funding_incomplete")),
                "funding": leg_funding(leg)}
        item["position_spans"] = [[str(p["entry_date"]), str(p["exit_date"])]
                                  for p in ex.get("position_list") or []]
        if leg.get("manifest"):
            item["candle_coverage_complete"] = leg["manifest"]["candle_coverage"]["complete"]
            item["funding_coverage_complete"] = leg["manifest"]["funding_coverage"].get("complete")
            item["rejected_entries"] = ex.get("rejected_entries")
        per[ds] = item
        for p in ex.get("position_list") or []:
            positions.append(dict(p, dataset=ds))
    vals = list(per.values())
    out.update({
        "mean_return_pct": round(statistics.mean(v["return_pct"] for v in vals), 4),
        "mean_sharpe": round(statistics.mean(v["sharpe"] for v in vals), 4),
        "mean_ddadj": round(statistics.mean(v["ddadj"] for v in vals), 4),
        "worst_max_dd_pct": round(min(v["max_dd_pct"] for v in vals), 4),
        "trades": sum(v["trades"] for v in vals),
        "positions": len(positions),
        "long_positions": sum(1 for p in positions if p["side"] == "long"),
        "short_positions": sum(1 for p in positions if p["side"] == "short"),
        "clusters": position_clusters(positions),
        "fee_drag_pct": round(statistics.mean(v["fee_drag_pct"] for v in vals), 4),
        "funding_drag_pct": round(statistics.mean(v["funding_drag_pct"] for v in vals), 4),
        "net_pnl_usd": round(sum(v["net_pnl_usd"] or 0.0 for v in vals), 4),
        "liquidated_legs": sum(1 for v in vals if v["liquidated"]),
        "funding_incomplete_legs": sum(1 for v in vals if v["funding_incomplete"]),
        "funding_fetched_legs": sum(1 for v in vals if v["funding"]["source"] == "fetched"),
        "coverage_complete": (all(v.get("candle_coverage_complete", True) and v.get("funding_coverage_complete", True)
                                  for v in vals)),
        "per_dataset": per,
    })
    return out


def brief(summary: dict) -> dict:
    return {k: v for k, v in summary.items() if k != "per_dataset"}


def index_units(records: list) -> dict:
    idx = {}
    for rec in records:
        u = rec["unit"]
        ds = f"{proxy_symbol(u['coin'])} {u['interval']}" if u["group"] == "proxy" else f"{u['coin']} {u['interval']}"
        for arm, by_cost in rec["legs"].items():
            for ck, leg in by_cost.items():
                idx[(u["group"], u["window"], arm, ck, ds)] = leg
        for ck, bar in rec["incumbent_bars"].items():
            idx[(u["group"], u["window"], "__bar__", ck, ds)] = bar
        idx[(u["group"], u["window"], "__range__", "-", ds)] = rec["range"]
    return idx


def set_members(proto: dict, name: str) -> list:
    return list(proto["dataset_sets"][name])


def proxy_summary(proto, idx, window, arm, datasets, bar_window=None) -> dict:
    legs, bars = {}, {}
    for coin, tf in datasets:
        ds = f"{proxy_symbol(coin)} {tf}"
        leg = idx.get(("proxy", window, arm, "1", ds))
        if leg is None and ("proxy", window, "__range__", "-", ds) not in idx:
            continue
        legs[ds] = leg
        bars[ds] = idx.get(("proxy", bar_window or window, "__bar__", "1", ds))
    return summarize(legs, bars, proto["capital_usd"])


def manifest_summary(proto, idx, group, window, arm, cost, datasets) -> dict:
    legs, bars = {}, {}
    ck = f"{cost:g}"
    for coin, tf in datasets:
        ds = f"{coin} {tf}"
        if (group, window, arm, ck, ds) not in idx:
            continue
        legs[ds] = idx[(group, window, arm, ck, ds)]
        bars[ds] = idx.get((group, window, "__bar__", ck, ds))
    return summarize(legs, bars, proto["capital_usd"])


def in_set(proto: dict, set_name: str, datasets: list) -> list:
    tfs = set(proto["dataset_sets"][set_name])
    return [d for d in datasets if d[1] in tfs]


def selection_from(proto: dict, cov: dict, idx: dict) -> dict:
    rule = proto["selection_rule"]
    w = proto["proxy"]["selection_window"]
    out = {"window": w, "sets": {}}
    for set_name in sorted(proto["dataset_sets"]):
        ds = in_set(proto, set_name, proxy_datasets(cov, w))
        arms = {a: brief(proxy_summary(proto, idx, w, a, ds)) for a in config_arms(proto)}
        shortlist, checks = [], {}
        for arm, spec in sorted(proto["arms"].items()):
            if spec["role"] != "gate":
                continue
            ctrl = arms[spec["control"]]
            s = arms[arm]
            c = {
                "beats_control_sharpe": s["mean_sharpe"] is not None and ctrl["mean_sharpe"] is not None
                and s["mean_sharpe"] > ctrl["mean_sharpe"],
                "beats_control_ddadj": s["mean_ddadj"] is not None and ctrl["mean_ddadj"] is not None
                and s["mean_ddadj"] > ctrl["mean_ddadj"],
                "not_degenerate": s["m1_verdict"] not in ("degenerate", "no data"),
                "min_clusters": s["clusters"] >= rule["min_is_clusters"],
                "no_funding_incomplete": s["funding_incomplete_legs"] == 0,
            }
            checks[arm] = c
            if all(c.values()):
                shortlist.append(arm)
        primary = [a for a in shortlist if proto["arms"][a]["family"] == "primary"]
        best = max(primary, key=lambda a: (arms[a]["mean_sharpe"], arms[a]["mean_ddadj"], a)) if primary else None
        secondary = [a for a in shortlist if proto["arms"][a]["family"] != "primary"]
        best2 = max(secondary, key=lambda a: (arms[a]["mean_sharpe"], arms[a]["mean_ddadj"], a)) if secondary else None
        out["sets"][set_name] = {"datasets": [f"{c} {t}" for c, t in ds], "arms": arms, "checks": checks,
                                 "shortlist": shortlist, "primary_arm": best, "policy_family_arm": best2}
    return out


def select_cmd(cache_db: str, work_dir: str, workers: int) -> None:
    proto = protocol()
    held = require_lock()
    cov = load_json(COVERAGE_PATH)
    if os.path.exists(SELECTION_PATH):
        raise SystemExit("selection.json exists; the selection is written once")
    frozen, cache_db = prepare_offline(cache_db, work_dir)
    probe = network_probe()
    records = run_units(select_units(proto, cov), "select", work_dir, cache_db, workers)
    idx = index_units(records)
    sel = selection_from(proto, cov, idx)
    sel.update({"protocol_sha256": protocol_sha256(), "protocol_lock": held, "coverage_sha256": sha256_file(COVERAGE_PATH),
                "source_commit": git_head(), "engine_sha256": engine_sha256(), "arms_sha256": arms_sha256(proto),
                "network_probe": probe, "network_blocked_while_scoring": True,
                "written_at_utc": time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime()),
                "unit_seconds": {unit_id(r["unit"]): r["seconds"] for r in records},
                "effective_ranges": {unit_id(r["unit"]): r["range"] for r in records}})
    write_json(SELECTION_PATH, sel)
    for name, s in sorted(sel["sets"].items()):
        print(f"set {name}: shortlist {s['shortlist']} primary {s['primary_arm']} policy {s['policy_family_arm']}")
    print(f"wrote {SELECTION_PATH}")


def require_selection() -> dict:
    if not os.path.exists(SELECTION_PATH):
        raise SystemExit("selection.json is missing; run `run_study.py select` first")
    sel = load_json(SELECTION_PATH)
    if sel["protocol_sha256"] != protocol_sha256():
        raise SystemExit("selection.json was written under a different protocol.json")
    return sel


def equality_check(idx: dict, a: str, b: str) -> dict:
    keys = ("return_pct", "sharpe", "max_dd_pct", "trades")
    total, equal, diffs = 0, 0, []
    for (group, window, arm, ck, ds), leg in sorted(idx.items(), key=lambda kv: str(kv[0])):
        if arm != a:
            continue
        other = idx.get((group, window, b, ck, ds))
        if leg is None or other is None:
            continue
        total += 1
        same = all(leg[k] == other[k] for k in keys) and (
            (leg.get("execution") or {}).get("position_list") == (other.get("execution") or {}).get("position_list"))
        if same:
            equal += 1
        else:
            diffs.append(f"{group} {window} x{ck} {ds}")
    return {"arms": [a, b], "legs_compared": total, "legs_equal": equal, "differences": diffs}


def judge_cmd(cache_db: str, work_dir: str, workers: int) -> None:
    proto = protocol()
    held = require_lock()
    require_selection()
    cov = load_json(COVERAGE_PATH)
    frozen, cache_db = prepare_offline(cache_db, work_dir)
    probe = network_probe()
    log = load_json(RUN_LOG_PATH) if os.path.exists(RUN_LOG_PATH) else {"runs": []}
    started = time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime())
    log.setdefault("starts", []).append({"started_utc": started, "protocol_sha256": protocol_sha256(),
                                         "source_commit": git_head()})
    write_json(RUN_LOG_PATH, log)
    t0 = time.time()
    records = run_units(judge_units(proto, cov), "judge", work_dir, cache_db, workers)
    sel_records = run_units(select_units(proto, cov), "select", work_dir, cache_db, workers)
    idx = index_units(records + sel_records)
    capital = proto["capital_usd"]
    proxy = {}
    for set_name in sorted(proto["dataset_sets"]):
        proxy[set_name] = {}
        for w in [proto["proxy"]["selection_window"]] + proto["proxy"]["judged_windows"]:
            ds = in_set(proto, set_name, proxy_datasets(cov, w))
            arms = config_arms(proto) + (sorted(proto["reference_arms"]) if w in proto["reference_windows"] else [])
            proxy[set_name][w] = {a: proxy_summary(proto, idx, w, a, ds) for a in arms}
    venue, bridge = {}, {}
    w = proto["venue"]["verdict_window"]
    for set_name in sorted(proto["dataset_sets"]):
        tfs = proto["dataset_sets"][set_name]
        vds = [(c, tf) for tf in tfs for c in venue_datasets(cov, w, tf)]
        bds = [(c, tf) for tf in tfs for c in bridge_datasets(cov, w, tf)]
        venue[set_name] = {w: {}}
        bridge[set_name] = {w: {}}
        for label, cost in sorted(proto["venue"]["cost_multipliers"].items()):
            venue[set_name][w][label] = {a: manifest_summary(proto, idx, "venue", w, a, cost, vds)
                                         for a in config_arms(proto)}
            bridge[set_name][w][label] = {a: manifest_summary(proto, idx, "bridge", w, a, cost, bds)
                                          for a in config_arms(proto)}
    common = {}
    for label, cost in sorted(proto["venue"]["cost_multipliers"].items()):
        for group in ("venue", "bridge"):
            for tf, w in sorted(proto["venue"]["common_windows"].items()):
                pick = venue_datasets if group == "venue" else bridge_datasets
                ds = [(c, tf) for c in pick(cov, w, tf)]
                common.setdefault(group, {}).setdefault(f"{tf} {w}", {})[label] = {
                    a: brief(manifest_summary(proto, idx, group, w, a, cost, ds)) for a in config_arms(proto)}
    audit_rows = {}
    headline = load_json(os.path.join(REPO, proto["audit_rerun"]["headline"]))
    for key in proto["audit_rerun"]["datasets"]:
        for arm in sorted(proto["audit_rerun"]["arms"]):
            leg = idx.get(("proxy", "audit", arm, "1", key))
            committed = headline["candidates"][proto["audit_rerun"]["arms"][arm]][key]
            audit_rows[f"{arm} {key}"] = {
                "rerun": {k: leg[k] for k in ("sharpe", "return_pct", "max_dd_pct", "trades")} if leg else None,
                "committed": {k: committed[k] for k in ("sharpe", "return_pct", "max_dd_pct", "trades")},
                "range": idx.get(("proxy", "audit", "__range__", "-", key)),
                "difference": ({k: round(leg[k] - committed[k], 4) for k in ("sharpe", "return_pct", "max_dd_pct",
                                                                               "trades")} if leg else None)}
    ranges = {}
    for rec in records + sel_records:
        ranges[unit_id(rec["unit"])] = rec["range"]
    fetched = sum(1 for k, v in idx.items() if k[2] not in ("__bar__", "__range__") and v is not None and
                  leg_funding(v).get("source") == "fetched")
    out = {
        "study": proto["study"], "issue": proto["issue"], "protocol_sha256": protocol_sha256(), "protocol_lock": held,
        "selection_sha256": sha256_file(SELECTION_PATH), "coverage_sha256": sha256_file(COVERAGE_PATH),
        "manifest_sha256": sha256_file(STUDY_MANIFEST_PATH),
        "offline_manifests_sha256": {n: sha256_file(os.path.join(HERE, n)) for n in sorted(MANIFEST_FILES.values())},
        "source_commit": git_head(), "engine_sha256": engine_sha256(), "arms_sha256": arms_sha256(proto),
        "network_blocked_while_scoring": True, "network_probe": probe, "funding_fetched_legs": fetched,
        "warmup_bars": proto["warmup_bars"], "capital_usd": capital,
        "proxy": proxy, "venue": venue, "bridge": bridge, "common_window": common, "audit_rerun": audit_rows,
        "policy_gate_p350_equality": equality_check(idx, "policy_gate_p350", "gate_p350"),
        "effective_ranges": dict(sorted(ranges.items())),
        "unit_seconds": {unit_id(r["unit"]): r["seconds"] for r in records},
        "runtime_seconds": round(time.time() - t0, 1),
    }
    if fetched:
        raise SystemExit(f"{fetched} legs fetched funding while scoring")
    write_json(RESULTS_PATH, out)
    log["runs"].append({"started_utc": started, "finished_utc": time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime()),
                        "protocol_sha256": protocol_sha256(), "selection_sha256": sha256_file(SELECTION_PATH),
                        "source_commit": git_head(), "units": len(records),
                        "units_scored_this_run": sum(1 for r in records if r.get("finished_utc", "") >= started)})
    write_json(RUN_LOG_PATH, log)
    print(f"wrote {RESULTS_PATH}; judge runs logged: {len(log['runs'])}")


def fidelity_units(proto: dict, cov: dict, sel: dict) -> list:
    gate_windows = {"composite_medium"}
    for s in sel["sets"].values():
        for arm in s["shortlist"]:
            gate_windows.add(proto["arms"][arm]["gate_window"])
    units = []
    for w in proto["fidelity"]["proxy_windows"]:
        for coin, tf in proxy_datasets(cov, w):
            for gw in sorted(gate_windows):
                units.append({"group": "proxy", "interval": tf, "coin": coin, "window": w, "regime_window": gw})
    for tf in proto["intervals"]:
        for w in proto["fidelity"]["venue_windows"][tf]:
            for coin in venue_datasets(cov, w, tf):
                for gw in sorted(gate_windows):
                    units.append({"group": "venue", "interval": tf, "coin": coin, "window": w, "regime_window": gw})
    return units


def regime_window_spec(proto: dict, name: str) -> dict:
    from regime import _normalize_spec
    cfgs = arm_configs(proto)
    for cfg in cfgs.values():
        if name in cfg["regime"]["windows"]:
            return _normalize_spec(copy.deepcopy(cfg["regime"]["windows"][name]))
    raise SystemExit(f"regime window {name!r} is in no arm config")


def fidelity_frames(proto: dict, unit: dict, manifests: dict):
    import pandas as pd
    warm = proto["warmup_bars"]
    if unit["group"] == "proxy":
        start, end = study_windows(proto)[unit["window"]]
        full = candle_frame(candle_file(unit["coin"], unit["interval"]))
        step = TF_MS[unit["interval"]]
        s, e = pd.Timestamp(start), pd.Timestamp(end)
        ext = full[(full.index >= s - pd.Timedelta(milliseconds=step * warm)) & (full.index < e)]
        window = ext[ext.index >= s]
        return window, ext
    import offline_manifest as om
    m = manifests[("venue", unit["interval"])]
    ds = next(d for d in m["datasets"] if d["coin"] == unit["coin"])
    ext, win, _ = om.window_frame(m, ds, unit["window"])
    window = om.slice_window(ext, win)
    return window, ext


def fidelity_engine_labels(proto: dict, unit: dict, ext, labels: MutableMapping):
    from regime_label_columns import label_cache_key, label_frame_sha256
    arm = proto["fidelity"]["engine_arm_for_window"][unit["regime_window"]]
    cand = build_arm_candidate(proto, arm, unit["coin"], unit["interval"])
    plan = cand["regime_label_windows"]
    spec = (plan.get("windows_spec") or {}).get(unit["regime_window"])
    key = label_cache_key(label_frame_sha256(ext), int(plan["limit"]), int(plan.get("period") or 14),
                          float(plan.get("adx_threshold") or 20.0), unit["regime_window"], spec)
    if key not in labels:
        return None, plan["limit"]
    return labels[key][""], plan["limit"]


def run_fidelity_unit(task):
    unit, path, fingerprint = task
    if checkpoint_fresh(path, fingerprint) is not None:
        return {"unit": unit, "status": "cached"}
    from regime import compute_regime_composite
    from regime_bounded_window_validate import validate_frames
    proto = protocol()
    t0 = time.time()
    manifests = load_manifests(verify=True)
    window, ext = fidelity_frames(proto, unit, manifests)
    spec = regime_window_spec(proto, unit["regime_window"])
    if spec["classifier"] != "composite":
        raise SystemExit("fidelity rows cover composite windows only")
    lookback = proto["fidelity"]["lookback"]
    eval_start = len(ext) - len(window)
    rep = validate_frames(window, ext, eval_start, None, incumbent_period=spec["period"],
                          thresholds=spec["thresholds"], lookback=lookback,
                          agreement_threshold=proto["fidelity"]["agreement_threshold"],
                          min_agreement_bars=proto["fidelity"]["min_agreement_bars"])
    drift = rep["handrule"]["label_drift"]
    engine, limit = fidelity_engine_labels(proto, unit, ext, DiskLabelCache(label_cache_root(_FID["work_dir"])))
    check = {"engine_labels_found": engine is not None, "sampled": 0, "equal": 0, "mismatches": []}
    if engine is not None:
        every = proto["fidelity"]["engine_check_every"]
        for i in range(eval_start, len(ext), every):
            if i + 1 < limit or not engine[i]:
                continue
            w = ext.iloc[i - lookback + 1:i + 1]
            lab = compute_regime_composite(w, period=spec["period"], thresholds=spec["thresholds"], tail=1)["regime"]
            got = str(lab.iloc[-1]) if len(lab) else ""
            check["sampled"] += 1
            if got == engine[i]:
                check["equal"] += 1
            elif len(check["mismatches"]) < 10:
                check["mismatches"].append({"bar": str(ext.index[i]), "engine": engine[i], "validator": got})
    shortest = min(lookback, eval_start + 1)
    out = {"unit": unit, "n": drift["n"], "agreement": drift["agreement"], "disagreements": drift["disagreements"],
           "transitions": drift["transitions"],
           "passes": drift["n"] >= proto["fidelity"]["min_agreement_bars"]
           and drift["agreement"] >= proto["fidelity"]["agreement_threshold"],
           "lookback": lookback, "shortest_bounded_slice": shortest, "eval_bars": len(window),
           "first_bar": str(window.index[0]), "last_bar": str(window.index[-1]),
           "period": spec["period"], "thresholds": spec["thresholds"], "engine_check": check,
           "seconds": round(time.time() - t0, 1), "fingerprint": fingerprint}
    write_json(path, out)
    return {"unit": unit, "status": "scored", "seconds": out["seconds"]}


_FID = {}


def fidelity_init(work_dir: str) -> None:
    for p in (os.path.join(REPO, "shared_tools"), BACKTEST):
        if p not in sys.path:
            sys.path.insert(0, p)
    block_network()
    _FID["work_dir"] = work_dir


def fidelity_cmd(work_dir: str, workers: int) -> None:
    proto = protocol()
    require_lock()
    sel = require_selection()
    if not os.path.exists(RESULTS_PATH):
        raise SystemExit("results.json is missing; run judge first")
    cov = load_json(COVERAGE_PATH)
    verify_frozen()
    block_network()
    units = fidelity_units(proto, cov, sel)
    ckdir = os.path.join(work_dir, "ckpt", "fidelity")
    os.makedirs(ckdir, exist_ok=True)
    proto_sha, eng, arms = protocol_sha256(), engine_sha256(), arms_sha256(proto)
    lab, val = labeller_sha256(), files_digest(VALIDATOR_FILES)
    tasks = []
    for u in units:
        name = f"{u['group']}_{u['interval']}_{u['coin']}_{u['window']}_{u['regime_window']}.json"
        tasks.append((u, os.path.join(ckdir, name), fidelity_fingerprint(u, proto_sha, eng, arms, lab, val)))
    ctx = multiprocessing.get_context("spawn")
    with ctx.Pool(processes=workers, initializer=fidelity_init, initargs=(work_dir,)) as pool:
        for i, res in enumerate(pool.imap_unordered(run_fidelity_unit, tasks), 1):
            u = res["unit"]
            print(f"[fidelity] {i}/{len(tasks)} {u['group']} {u['coin']} {u['interval']} {u['window']} "
                  f"{u['regime_window']} {res['status']}", flush=True)
    rows = [{k: v for k, v in load_json(t[1]).items() if k != "fingerprint"} for t in tasks]
    rows.sort(key=lambda r: (r["unit"]["group"], r["unit"]["coin"], r["unit"]["interval"], r["unit"]["window"],
                             r["unit"]["regime_window"]))
    bad = [r for r in rows if r["engine_check"]["engine_labels_found"] and
           r["engine_check"]["equal"] != r["engine_check"]["sampled"]]
    missing = [r for r in rows if not r["engine_check"]["engine_labels_found"]]
    write_json(FIDELITY_PATH, {
        "protocol_sha256": protocol_sha256(), "selection_sha256": sha256_file(SELECTION_PATH),
        "source_commit": git_head(), "network_blocked": True, "rows": rows,
        "engine_check": {"rows_checked": len(rows) - len(missing), "rows_without_engine_labels": len(missing),
                         "rows_with_mismatch": len(bad),
                         "sampled_bars": sum(r["engine_check"]["sampled"] for r in rows)}})
    if bad:
        raise SystemExit(f"engine cached labels disagree with the validator's bounded labels in {len(bad)} rows")
    print(f"wrote {FIDELITY_PATH}: {len(rows)} rows, {sum(1 for r in rows if r['passes'])} pass")


def failing_fidelity(proto: dict, fid: dict, arm: str) -> set:
    windows = {"composite_medium", proto["arms"][arm]["gate_window"]}
    out = set()
    for r in fid["rows"]:
        u = r["unit"]
        if u["regime_window"] in windows and not r["passes"]:
            out.add((u["coin"], u["interval"], u["group"]))
    return out


def pooled_clusters(per: dict, keys: list) -> int:
    spans = [{"entry_date": e, "exit_date": x} for k in keys if k in per for e, x in per[k]["position_spans"]]
    return position_clusters(spans)


def mean_block(per: dict, keys: list):
    vals = [per[k] for k in keys if k in per and per[k] is not None]
    if not vals:
        return None
    return {"mean_sharpe": statistics.mean(v["sharpe"] for v in vals),
            "mean_ddadj": statistics.mean(v["ddadj"] for v in vals),
            "mean_return_pct": statistics.mean(v["return_pct"] for v in vals),
            "worst_max_dd_pct": min(v["max_dd_pct"] for v in vals),
            "complete": all(v.get("candle_coverage_complete", True) and v.get("funding_coverage_complete", True)
                            and not v["funding_incomplete"] for v in vals),
            "datasets": len(vals), "clusters": pooled_clusters(per, keys)}


def venue_verdict(proto: dict, res: dict, set_name: str, arm: str, keys: list) -> dict:
    rule = proto["venue_rule"]
    ctrl = proto["arms"][arm]["control"]
    w = proto["venue"]["verdict_window"]
    out = {"window": w, "datasets": keys}
    blocks = {}
    for label in sorted(proto["venue"]["cost_multipliers"]):
        summ = res["venue"][set_name][w][label]
        blocks[label] = {"arm": mean_block(summ[arm]["per_dataset"], keys),
                         "control": mean_block(summ[ctrl]["per_dataset"], keys)}
    a1, c1 = blocks["base"]["arm"], blocks["base"]["control"]
    a2, c2 = blocks["stress"]["arm"], blocks["stress"]["control"]
    out.update({"arm_x1": a1, "control_x1": c1, "arm_x2": a2, "control_x2": c2})
    present = all(x is not None and x["datasets"] == len(keys) for x in (a1, c1, a2, c2))
    if not keys or not present or not all(x["complete"] for x in (a1, c1, a2, c2)):
        out.update({"outcome": "inconclusive", "reason": "venue coverage incomplete or missing legs"})
    elif a1["clusters"] < rule["min_clusters"]:
        out.update({"outcome": "inconclusive",
                    "reason": f"gate arm venue clusters {a1['clusters']} < {rule['min_clusters']}"})
    elif (a1["mean_sharpe"] > c1["mean_sharpe"] and a1["mean_ddadj"] > c1["mean_ddadj"]
          and a2["mean_sharpe"] > c2["mean_sharpe"] and a2["mean_ddadj"] > c2["mean_ddadj"]):
        out.update({"outcome": "pass", "reason": "beats control on mean Sharpe and mean ddadj at cost x1 and x2"})
    elif a1["mean_sharpe"] < c1["mean_sharpe"] and a1["mean_ddadj"] < c1["mean_ddadj"]:
        out.update({"outcome": "fail", "reason": "loses to control on mean Sharpe and mean ddadj at cost x1"})
    else:
        out.update({"outcome": "inconclusive", "reason": "mixed result against control"})
    return out


def adopt_set(proto: dict, sel: dict, res: dict, fid: dict, cov: dict, set_name: str) -> dict:
    arm = sel["sets"][set_name]["primary_arm"]
    out = {"set": set_name, "arm": arm}
    if not arm:
        out.update({"adopted": False, "reason": "no primary-family gate arm met the frozen is shortlist rule"})
        return out
    ctrl = proto["arms"][arm]["control"]
    rule = proto["adoption_rule"]
    bad_fid = failing_fidelity(proto, fid, arm)
    excluded = {}
    datasets = in_set(proto, set_name, proxy_datasets(cov, proto["proxy"]["selection_window"]))
    for coin, tf in datasets:
        ds = f"{proxy_symbol(coin)} {tf}"
        why = []
        for w in rule["funding_windows"]:
            for a in (arm, ctrl):
                per = res["proxy"][set_name][w][a]["per_dataset"].get(ds)
                if per is None:
                    why.append(f"{a} {w} leg missing")
                elif per["funding_incomplete"]:
                    why.append(f"{a} {w} funding_incomplete")
        for group in ("proxy", "venue"):
            if (coin, tf, group) in bad_fid:
                why.append(f"failing {group} live-label fidelity row")
        if why:
            excluded[ds] = why
    kept = [(c, tf) for c, tf in datasets if f"{proxy_symbol(c)} {tf}" not in excluded]
    keys = [f"{proxy_symbol(c)} {tf}" for c, tf in kept]
    out.update({"excluded": excluded, "datasets": keys})
    oos_w, st_w = rule["judged_window"], rule["stitched_window"]
    oa = mean_block(res["proxy"][set_name][oos_w][arm]["per_dataset"], keys)
    oc = mean_block(res["proxy"][set_name][oos_w][ctrl]["per_dataset"], keys)
    sa = mean_block(res["proxy"][set_name][st_w][arm]["per_dataset"], keys)
    sc = mean_block(res["proxy"][set_name][st_w][ctrl]["per_dataset"], keys)
    venue_keys = [f"{c} {tf}" for c, tf in kept]
    venue = venue_verdict(proto, res, set_name, arm, venue_keys)
    checks = {
        "oos_beats_control_sharpe": bool(oa and oc and oa["mean_sharpe"] > oc["mean_sharpe"]),
        "oos_beats_control_ddadj": bool(oa and oc and oa["mean_ddadj"] > oc["mean_ddadj"]),
        "stitched_shallower_worst_dd": bool(sa and sc and sa["worst_max_dd_pct"] > sc["worst_max_dd_pct"]),
        "stitched_mean_return_not_lower": bool(sa and sc and sa["mean_return_pct"] >= sc["mean_return_pct"]),
        "oos_min_clusters": bool(oa) and oa["clusters"] >= rule["min_oos_clusters"],
        "venue_not_fail": venue["outcome"] != "fail",
    }
    out.update({"oos_arm": oa, "oos_control": oc, "stitched_arm": sa, "stitched_control": sc,
                "venue": venue, "checks": checks, "adopted": all(checks.values()) and bool(kept)})
    out["reason"] = ("every frozen adoption check is true" if out["adopted"] else
                     "false: " + ", ".join(sorted(k for k, v in checks.items() if not v)))
    return out


def suggest_cmd() -> None:
    proto = protocol()
    require_lock()
    sel = require_selection()
    res = load_json(RESULTS_PATH)
    fid = load_json(FIDELITY_PATH)
    cov = load_json(COVERAGE_PATH)
    if res["selection_sha256"] != sha256_file(SELECTION_PATH) or fid["selection_sha256"] != sha256_file(SELECTION_PATH):
        raise SystemExit("results.json or fidelity.json was written under a different selection.json")
    sets = {s: adopt_set(proto, sel, res, fid, cov, s) for s in sorted(proto["dataset_sets"])}
    adopted = [s for s in proto["adoption_order"] if sets[s]["adopted"]]
    chosen = []
    if "all" in adopted:
        chosen = ["all"]
    else:
        chosen = [s for s in adopted if s != "all"]
    verdict = {"protocol_sha256": protocol_sha256(), "results_sha256": sha256_file(RESULTS_PATH),
               "fidelity_sha256": sha256_file(FIDELITY_PATH), "sets": sets, "adopted_sets": chosen}
    if chosen:
        cfg = load_json(DEPLOYED_PATH)
        gated = []
        add_windows = {}
        for s in chosen:
            arm = sets[s]["arm"]
            rule = proto["arm_configs"][proto["arms"][arm]["config_name"]]
            add_windows.update(rule.get("add_regime_windows") or {})
            for ds in sets[s]["datasets"]:
                symbol, tf = ds.split(" ")
                sid = strategy_id(symbol.split("/")[0], tf)
                base = next(sc for sc in cfg["strategies"] if sc["id"] == sid)
                g = copy.deepcopy(base)
                g["id"] = sid + "-gated"
                for key, value in sorted((rule.get("strategy_overrides") or {}).items()):
                    g[key] = copy.deepcopy(value)
                gated.append(g)
        for key, spec in sorted(add_windows.items()):
            cfg["regime"]["windows"][key] = copy.deepcopy(spec)
        cfg["strategies"] = sorted(cfg["strategies"] + gated, key=lambda sc: sc["id"])
        write_json(SUGGESTED_PATH, cfg)
        verdict["outcome"] = "gate adopted for " + ", ".join(
            f"{s} ({sets[s]['arm']}: {', '.join(sets[s]['datasets'])})" for s in chosen)
        verdict["suggested_config"] = os.path.basename(SUGGESTED_PATH)
        verdict["gated_ids"] = sorted(g["id"] for g in gated)
    else:
        if os.path.exists(SUGGESTED_PATH):
            os.remove(SUGGESTED_PATH)
        verdict["outcome"] = "negative result"
        verdict["suggested_config"] = None
        verdict["reason"] = "; ".join(f"{s}: {sets[s]['reason']}" for s in sorted(sets))
    write_json(VERDICT_PATH, verdict)
    print(f"verdict: {verdict['outcome']}")


def time_leg_cmd(cache_db: str, work_dir: str) -> None:
    proto = protocol()
    require_lock()
    cov = load_json(COVERAGE_PATH)
    frozen, cache_db = prepare_offline(cache_db, work_dir)
    worker_init(cache_db, work_dir)
    import eval_windows as ew
    coin, tf = proto["timing"]["coin"], proto["timing"]["interval"]
    w = proto["proxy"]["selection_window"]
    window = study_windows(proto)[w]
    rng = proxy_effective_range(proxy_symbol(coin), tf, window, proto["warmup_bars"])
    calls = []
    import regime
    real = regime.bounded_window_labels

    def timed(df, **kw):
        t = time.time()
        r = real(df, **kw)
        calls.append({"bars": len(df), "windows": sorted((kw.get("windows_spec") or {}).keys()),
                      "seconds": time.time() - t})
        return r
    regime.bounded_window_labels = timed
    t0 = time.time()
    labels = DiskLabelCache(label_cache_root(work_dir, "labels_timing"))
    cand = build_arm_candidate(proto, "control", coin, tf)
    leg = ew.run_candidate_leg(_WORKER["reg"], cand, proxy_symbol(coin), tf, window, proto["capital_usd"],
                               keep_trades=True, funding_mode=proto["funding_mode"], warmup_bars=proto["warmup_bars"],
                               regime_label_cache=labels)
    total = time.time() - t0
    regime.bounded_window_labels = real
    label_s = sum(c["seconds"] for c in calls)
    bars = sum(c["bars"] for c in calls)
    per_window_bar = label_s / max(1, sum(c["bars"] * len(c["windows"]) for c in calls))
    units = select_units(proto, cov) + judge_units(proto, cov)
    proj_bars = 0
    for u in units:
        if u.get("audit") or u.get("tag") == "refs":
            continue
        per = 48 if u["interval"] == "30m" else 24
        if u["group"] == "proxy":
            import pandas as pd
            s, e = u["range"]
            n = (pd.Timestamp(e) - pd.Timestamp(s)).days * per + proto["warmup_bars"]
            nwin = 3
        else:
            n = 5000
            nwin = 3
        proj_bars += n * nwin
    fid = 0
    for wname in proto["fidelity"]["proxy_windows"]:
        for coin2, tf2 in proxy_datasets(cov, wname):
            s, e = study_windows(proto)[wname]
            import pandas as pd
            fid += (pd.Timestamp(e) - pd.Timestamp(s)).days * (48 if tf2 == "30m" else 24) * 2
    for tf2 in proto["intervals"]:
        for wname in proto["fidelity"]["venue_windows"][tf2]:
            fid += len(venue_datasets(cov, wname, tf2)) * 4600 * 2
    out = {"dataset": f"{coin} {tf}", "window": w, "effective_range": rng, "leg_seconds": round(total, 1),
           "label_calls": calls, "label_seconds": round(label_s, 1), "label_frame_bars": bars,
           "seconds_per_window_bar": round(per_window_bar, 5), "leg_trades": leg["trades"],
           "projected_label_window_bars": proj_bars,
           "projected_fidelity_bars_upper": fid,
           "projected_process_hours_labels": round(proj_bars * per_window_bar / 3600, 1),
           "projected_process_hours_fidelity_upper": round(fid * per_window_bar / 3600, 1),
           "machine": {"cpus": os.cpu_count(), "load_avg": list(os.getloadavg())},
           "measured_at_utc": time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime())}
    write_json(TIMING_PATH, out)
    print(json.dumps({k: v for k, v in out.items() if k != "label_calls"}, indent=2))


def cache_parity_cmd(cache_db: str, work_dir: str) -> None:
    proto = protocol()
    frozen, cache_db = prepare_offline(cache_db, work_dir)
    worker_init(cache_db, work_dir)
    import eval_windows as ew
    coin, tf = proto["cache_parity"]["coin"], proto["cache_parity"]["interval"]
    w = proto["cache_parity"]["window"]
    window = study_windows(proto)[w]
    rows = {}
    for arm in proto["cache_parity"]["arms"]:
        cand = build_arm_candidate(proto, arm, coin, tf)
        legs = {}
        for mode in ("uncached", "cached"):
            t0 = time.time()
            cache = DiskLabelCache(label_cache_root(work_dir)) if mode == "cached" else None
            leg = ew.run_candidate_leg(_WORKER["reg"], copy.deepcopy(cand), proxy_symbol(coin), tf, window,
                                       proto["capital_usd"], keep_trades=True, funding_mode=proto["funding_mode"],
                                       warmup_bars=proto["warmup_bars"], regime_label_cache=cache)
            legs[mode] = {"json": json.dumps(leg, sort_keys=True, default=str), "seconds": round(time.time() - t0, 1)}
        same = legs["uncached"]["json"] == legs["cached"]["json"]
        rows[arm] = {"identical": same, "sha256_uncached": sha256_bytes(legs["uncached"]["json"].encode()),
                     "sha256_cached": sha256_bytes(legs["cached"]["json"].encode()),
                     "seconds": {m: v["seconds"] for m, v in legs.items()},
                     "trades": json.loads(legs["cached"]["json"])["trades"]}
        print(f"{arm}: identical {same} {rows[arm]['seconds']}", flush=True)
    out = {"dataset": f"{proxy_symbol(coin)} {tf}", "window": w, "range": list(window),
           "warmup_bars": proto["warmup_bars"], "rows": rows, "identical": all(r["identical"] for r in rows.values()),
           "engine_sha256": engine_sha256(), "source_commit": git_head()}
    write_json(CACHE_PARITY_PATH, out)
    if not out["identical"]:
        raise SystemExit("cached and uncached legs differ")
    print(f"wrote {CACHE_PARITY_PATH}")


def main(argv=None) -> int:
    p = argparse.ArgumentParser(description="#2014 breakout trend-quality gate study (suggest-only)")
    sub = p.add_subparsers(dest="cmd", required=True)
    b = sub.add_parser("build-arms")
    b.add_argument("--check", action="store_true")
    a = sub.add_parser("acquire-proxy")
    a.add_argument("--cache-db", required=True)
    sub.add_parser("verify-frozen")
    sub.add_parser("coverage")
    sub.add_parser("lock")
    sub.add_parser("hash")
    for name in ("cache-parity", "time-leg", "select", "judge"):
        q = sub.add_parser(name)
        q.add_argument("--cache-db", required=True)
        q.add_argument("--work-dir", required=True)
        if name in ("select", "judge"):
            q.add_argument("--workers", type=int, default=4)
    f = sub.add_parser("fidelity")
    f.add_argument("--work-dir", required=True)
    f.add_argument("--workers", type=int, default=4)
    sub.add_parser("suggest")
    sub.add_parser("render")
    args = p.parse_args(argv)
    if args.cmd == "build-arms":
        build_arms(args.check)
    elif args.cmd == "acquire-proxy":
        acquire_proxy(args.cache_db)
    elif args.cmd == "verify-frozen":
        frozen = verify_frozen()
        print(f"verified {len(frozen['proxy']['files'])} proxy files and {len(frozen['manifests'])} manifests")
    elif args.cmd == "coverage":
        coverage()
    elif args.cmd == "lock":
        lock()
    elif args.cmd == "hash":
        print(protocol_sha256())
    elif args.cmd == "cache-parity":
        cache_parity_cmd(args.cache_db, args.work_dir)
    elif args.cmd == "time-leg":
        time_leg_cmd(args.cache_db, args.work_dir)
    elif args.cmd == "select":
        select_cmd(args.cache_db, args.work_dir, args.workers)
    elif args.cmd == "judge":
        judge_cmd(args.cache_db, args.work_dir, args.workers)
    elif args.cmd == "fidelity":
        fidelity_cmd(args.work_dir, args.workers)
    elif args.cmd == "suggest":
        suggest_cmd()
    elif args.cmd == "render":
        from render_report import render
        render()
    return 0


if __name__ == "__main__":
    sys.exit(main())

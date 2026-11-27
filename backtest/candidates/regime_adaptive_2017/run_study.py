#!/usr/bin/env python3
"""Offline entry-filter study for regime_adaptive (issue 2017).

Scoring reads the hash-pinned tema_cross_2016 candle files, blocks network
access, and refuses a protocol or input hash that differs from the lock.
"""

from __future__ import annotations

import argparse
import gzip
import hashlib
import json
import math
import multiprocessing
import os
import socket
import statistics
import subprocess
import sys
import tempfile
import time
from datetime import datetime, timezone

HERE = os.path.dirname(os.path.abspath(__file__))
BACKTEST = os.path.abspath(os.path.join(HERE, "..", ".."))
REPO = os.path.abspath(os.path.join(BACKTEST, ".."))
TEMA_MANIFEST = os.path.join(BACKTEST, "candidates", "tema_cross_2016", "study_manifest.json")
PROTOCOL_PATH = os.path.join(HERE, "protocol.json")
LOCK_PATH = os.path.join(HERE, "protocol_lock.json")
MANIFEST_PATH = os.path.join(HERE, "study_manifest.json")
COVERAGE_PATH = os.path.join(HERE, "coverage.json")
SELECTION_PATH = os.path.join(HERE, "selection.json")
RESULTS_PATH = os.path.join(HERE, "results.json")
REPORT_PATH = os.path.join(HERE, "REPORT.md")
SUGGESTED_PATH = os.path.join(HERE, "suggested_config.json")
CACHE_ENV = "GO_TRADER_OHLCV_CACHE_DB"

for path in (BACKTEST, os.path.join(REPO, "shared_tools"), os.path.join(REPO, "shared_strategies", "open")):
    if path not in sys.path:
        sys.path.insert(0, path)

ENGINE_FILES = (
    "backtest/candidates/regime_adaptive_2017/run_study.py",
    "backtest/candidates/regime_adaptive_2017/protocol.json",
    "backtest/eval_windows.py",
    "backtest/backtester.py",
    "backtest/run_backtest.py",
    "backtest/htf_trend.py",
    "backtest/gross_edge_noise.py",
    "backtest/auto_suggest.py",
    "backtest/regime_stats.py",
    "backtest/regime_label_columns.py",
    "shared_strategies/open/regime_adaptive.py",
    "shared_strategies/close/trailing_tp_ratchet.py",
    "shared_tools/regime.py",
    "shared_tools/funding_fetcher.py",
    "shared_tools/data_fetcher.py",
    "shared_tools/storage.py",
)

TF_MS = {"30m": 1_800_000, "1h": 3_600_000, "4h": 14_400_000}
FILTER_FAMILIES = ("gate", "htf", "threshold")


def sha256_bytes(data: bytes) -> str:
    return hashlib.sha256(data).hexdigest()


def sha256_file(path: str) -> str:
    h = hashlib.sha256()
    with open(path, "rb") as fh:
        for chunk in iter(lambda: fh.read(1 << 20), b""):
            h.update(chunk)
    return h.hexdigest()


def now_utc() -> str:
    return datetime.now(timezone.utc).strftime("%Y-%m-%dT%H:%M:%SZ")


def dumps(obj) -> str:
    return json.dumps(obj, indent=2, sort_keys=True) + "\n"


def write_json(path: str, obj) -> None:
    tmp = path + ".tmp"
    with open(tmp, "w", encoding="utf-8") as fh:
        fh.write(dumps(obj))
    os.replace(tmp, path)


def load_json(path: str):
    with open(path, encoding="utf-8") as fh:
        return json.load(fh)


def protocol() -> dict:
    return load_json(PROTOCOL_PATH)


def protocol_sha256() -> str:
    return sha256_file(PROTOCOL_PATH)


def git_head() -> str:
    out = subprocess.check_output(["git", "-C", REPO, "rev-parse", "HEAD"], text=True)
    return out.strip()


def engine_sha256() -> dict:
    out = {}
    for rel in ENGINE_FILES:
        out[rel] = sha256_file(os.path.join(REPO, rel))
    return out


def engine_digest(eng: dict | None = None) -> str:
    eng = eng or engine_sha256()
    raw = json.dumps(eng, sort_keys=True).encode()
    return sha256_bytes(raw)


def arm_by_name(proto: dict, name: str) -> dict:
    for arm in proto["arms"]:
        if arm["name"] == name:
            return arm
    raise KeyError(name)


def symbol_of(coin: str, proto: dict) -> str:
    return f"{coin}/{proto['symbol_quote']}"


def coin_of(symbol: str) -> str:
    return symbol.split("/")[0]


def ms_iso(ms: int) -> str:
    return datetime.fromtimestamp(int(ms) / 1000, timezone.utc).strftime("%Y-%m-%dT%H:%M:%SZ")


def input_paths(proto: dict) -> list:
    root = os.path.join(REPO, proto["inputs"]["root"])
    coins = proto["symbols"]
    rows = []
    for coin in coins:
        for tf in proto["inputs"]["proxy_timeframes"]:
            rows.append({
                "role": "proxy_candles",
                "coin": coin,
                "timeframe": tf,
                "path": os.path.join(root, "proxy", f"{coin}_{tf}_candles.csv.gz"),
            })
        rows.append({
            "role": "proxy_funding",
            "coin": coin,
            "timeframe": None,
            "path": os.path.join(root, "proxy", f"{coin}_funding.csv.gz"),
        })
        for tf in proto["inputs"]["venue_timeframes"]:
            folder = f"venue_{tf}"
            rows.append({
                "role": "venue_candles",
                "coin": coin,
                "timeframe": tf,
                "path": os.path.join(root, folder, f"{coin}_{tf}_candles.csv.gz"),
            })
            rows.append({
                "role": "venue_funding",
                "coin": coin,
                "timeframe": tf,
                "path": os.path.join(root, folder, f"{coin}_funding.csv.gz"),
            })
    return rows


def gzip_ends(path: str) -> dict:
    first = last = None
    rows = 0
    with gzip.open(path, "rt") as fh:
        header = fh.readline()
        if "timestamp" not in header:
            raise SystemExit(f"{path} has no timestamp column")
        for line in fh:
            stamp = line.split(",", 1)[0]
            if first is None:
                first = stamp
            last = stamp
            rows += 1
    if first is None:
        raise SystemExit(f"{path} has no rows")
    return {"rows": rows, "first_ms": int(first), "last_ms": int(last),
            "first": ms_iso(int(first)), "last": ms_iso(int(last))}


def tema_funding_meta(rel_path: str) -> dict:
    key = rel_path.split("/data/", 1)[1]
    doc = load_json(TEMA_MANIFEST)
    meta = doc["files"].get(key)
    if not meta or meta.get("kind") != "funding":
        raise SystemExit(f"tema manifest has no funding metadata for {key}")
    return meta


def absent_pin(meta: dict) -> dict:
    pairs = meta.get("venue_absent_hours") or []
    hours = [int(h) for h, _obs in pairs]
    observed = [int(obs) for _h, obs in pairs]
    return {
        "venue_absent_count": len(hours),
        "venue_absent_sha256": sha256_bytes(json.dumps(hours).encode()),
        "observed_through_ms": max(observed) if observed else None,
        "stored_coverage_intervals": meta.get("stored_coverage_intervals") or [],
    }


def verify_inputs(write: bool) -> dict:
    proto = protocol()
    files = []
    for row in input_paths(proto):
        if not os.path.isfile(row["path"]):
            raise SystemExit(f"missing frozen input {row['path']}")
        ends = gzip_ends(row["path"])
        rel = os.path.relpath(row["path"], REPO)
        item = {
            "role": row["role"],
            "coin": row["coin"],
            "timeframe": row["timeframe"],
            "path": rel,
            "sha256": sha256_file(row["path"]),
            **ends,
        }
        if row["role"] == "proxy_funding":
            item.update(absent_pin(tema_funding_meta(rel)))
        files.append(item)
    doc = {
        "issue": proto["issue"],
        "source_study": proto["inputs"]["study"],
        "tema_manifest": os.path.relpath(TEMA_MANIFEST, REPO),
        "tema_manifest_sha256": sha256_file(TEMA_MANIFEST),
        "execution_spec": None,
        "files": files,
    }
    if write:
        if os.path.exists(LOCK_PATH):
            raise SystemExit("protocol_lock.json exists; verify-inputs will not rewrite the manifest")
        write_json(MANIFEST_PATH, doc)
        print(f"wrote {len(files)} input rows to study_manifest.json")
    elif not os.path.exists(MANIFEST_PATH):
        raise SystemExit("study_manifest.json is missing; run verify-inputs before lock")
    else:
        locked = load_json(MANIFEST_PATH)
        if (locked.get("files") != doc["files"]
                or locked.get("tema_manifest_sha256") != doc["tema_manifest_sha256"]):
            raise SystemExit("frozen inputs differ from study_manifest.json")
    return doc


def require_lock() -> dict:
    if not os.path.exists(LOCK_PATH):
        raise SystemExit("protocol_lock.json is missing; run lock before scoring")
    lock = load_json(LOCK_PATH)
    if lock.get("protocol_sha256") != protocol_sha256():
        raise SystemExit("protocol.json hash differs from the lock")
    if not os.path.exists(MANIFEST_PATH) or sha256_file(MANIFEST_PATH) != lock.get("manifest_sha256"):
        raise SystemExit("study_manifest.json hash differs from the lock")
    current = engine_sha256()
    if current != lock.get("engine_sha256"):
        raise SystemExit("evaluator file hashes differ from the lock")
    verify_inputs(write=False)
    return lock


def lock_cmd() -> None:
    if os.path.exists(SELECTION_PATH) or os.path.exists(RESULTS_PATH):
        raise SystemExit("selection or results already exist; refusing to lock after scoring")
    if not os.path.exists(MANIFEST_PATH):
        raise SystemExit("run verify-inputs before lock")
    doc = {
        "locked_at": now_utc(),
        "git_head": git_head(),
        "protocol_sha256": protocol_sha256(),
        "manifest_sha256": sha256_file(MANIFEST_PATH),
        "engine_sha256": engine_sha256(),
    }
    doc["engine_digest"] = engine_digest(doc["engine_sha256"])
    write_json(LOCK_PATH, doc)
    print(f"locked protocol {doc['protocol_sha256']} at {doc['locked_at']}")


def block_network() -> None:
    def refuse(*_a, **_k):
        raise RuntimeError("frozen study run: network access is blocked")

    socket.socket.connect = refuse
    socket.socket.connect_ex = refuse
    socket.create_connection = refuse
    socket.getaddrinfo = refuse
    import data_fetcher
    import funding_fetcher
    data_fetcher.fetch_full_history = refuse
    data_fetcher.fetch_ohlcv = refuse
    data_fetcher.get_exchange = refuse
    funding_fetcher._hl_adapter = refuse


def set_cache(path: str) -> str:
    path = os.path.abspath(path)
    default = os.path.join(REPO, "shared_tools", "trading_bot.db")
    if os.path.realpath(path) == os.path.realpath(default):
        raise SystemExit(f"refusing the default cache {default}")
    os.environ[CACHE_ENV] = path
    return path


def file_index(manifest: dict) -> dict:
    idx = {}
    for row in manifest["files"]:
        idx[(row["role"], row["coin"], row["timeframe"])] = row
    return idx


def coverage_cmd() -> None:
    require_lock()
    proto = protocol()
    manifest = load_json(MANIFEST_PATH)
    idx = file_index(manifest)
    import pandas as pd
    from eval_windows import warmup_load_start

    windows = {}
    for name, bounds in proto["windows"].items():
        start, end = bounds
        per = []
        for coin in proto["symbols"]:
            for tf in proto["inputs"]["proxy_timeframes"]:
                row = idx[("proxy_candles", coin, tf)]
                fund = idx[("proxy_funding", coin, None)]
                warm = warmup_load_start(start, tf, proto["warmup_bars"])
                warm_ms = int(pd.Timestamp(warm).timestamp() * 1000)
                end_ms = int(pd.Timestamp(end).timestamp() * 1000)
                need_last = end_ms - TF_MS[tf]
                candles_ok = row["first_ms"] <= warm_ms and row["last_ms"] >= need_last
                funding_ok = fund["first_ms"] <= warm_ms and fund["last_ms"] >= end_ms
                per.append({
                    "coin": coin,
                    "timeframe": tf,
                    "warmup_start": str(pd.Timestamp(warm)),
                    "candles_cover": candles_ok,
                    "funding_cover": funding_ok,
                    "candle_first": row["first"],
                    "candle_last": row["last"],
                    "funding_first": fund["first"],
                    "funding_last": fund["last"],
                })
        windows[name] = per

    native = []
    native_ok = True
    for coin in proto["symbols"]:
        for tf in proto["frames"]["perps"]["intervals"]:
            row = idx[("venue_candles", coin, tf)]
            fund = idx[("venue_funding", coin, tf)]
            covered = True
            gaps = []
            for name in proto["judgment_windows"]:
                start, end = proto["windows"][name]
                warm = warmup_load_start(start, tf, proto["warmup_bars"])
                warm_ms = int(pd.Timestamp(warm).timestamp() * 1000)
                end_ms = int(pd.Timestamp(end).timestamp() * 1000)
                ok = (row["first_ms"] <= warm_ms and row["last_ms"] >= end_ms - TF_MS[tf]
                      and fund["first_ms"] <= warm_ms and fund["last_ms"] >= end_ms)
                if not ok:
                    covered = False
                    gaps.append(name)
            native_ok = native_ok and covered
            native.append({
                "coin": coin,
                "timeframe": tf,
                "candle_first": row["first"],
                "candle_last": row["last"],
                "funding_first": fund["first"],
                "funding_last": fund["last"],
                "covers_judgment": covered,
                "uncovered_windows": gaps,
            })
    doc = {
        "protocol_sha256": protocol_sha256(),
        "native_covers_judgment": native_ok,
        "execution_spec": None,
        "windows": windows,
        "native": native,
    }
    write_json(COVERAGE_PATH, doc)
    print(f"coverage: native judgment cover={native_ok}")


def build_cache(cache_path: str) -> None:
    if not os.path.exists(MANIFEST_PATH):
        raise SystemExit("study_manifest.json is missing; run verify-inputs")
    cache_path = set_cache(cache_path)
    stamp_path = cache_path + ".stamp"
    manifest_sha = sha256_file(MANIFEST_PATH)
    if os.path.exists(cache_path) and os.path.exists(stamp_path) and open(stamp_path).read().strip() == manifest_sha:
        print(f"reusing cache {cache_path}")
        return
    for suffix in ("", "-wal", "-shm"):
        extra = cache_path + suffix
        if os.path.exists(extra):
            os.remove(extra)
    import pandas as pd
    import storage

    manifest = load_json(MANIFEST_PATH)
    storage.init_db(cache_path)
    for row in manifest["files"]:
        if row["role"] not in ("proxy_candles", "proxy_funding"):
            continue
        path = os.path.join(REPO, row["path"])
        if row["role"] == "proxy_candles":
            df = pd.read_csv(path, compression="gzip")
            df = df.astype({"timestamp": "int64", "open": float, "high": float, "low": float,
                            "close": float, "volume": float})
            symbol = f"{row['coin']}/USDT"
            storage.store_ohlcv(df, "binanceus", symbol, row["timeframe"], db_path=cache_path)
            print(f"cached candles {symbol} {row['timeframe']} {len(df)}")
        else:
            df = pd.read_csv(path, compression="gzip")
            records = [{"time": int(t), "rate": float(r)} for t, r in zip(df["timestamp"], df["rate"])]
            meta = tema_funding_meta(row["path"])
            pin = absent_pin(meta)
            if pin["venue_absent_sha256"] != row.get("venue_absent_sha256"):
                raise SystemExit(f"funding absent-hour hash changed for {row['coin']}")
            hours = [int(h) for h, _obs in meta["venue_absent_hours"]]
            storage.store_funding_fetch(
                "hyperliquid", row["coin"], records, hours,
                observed_through_ms=pin["observed_through_ms"], db_path=cache_path)
            for start_ms, end_ms in pin["stored_coverage_intervals"]:
                storage.store_funding_coverage(
                    "hyperliquid", row["coin"], int(start_ms), int(end_ms), db_path=cache_path)
            print(f"cached funding {row['coin']} {len(records)} absent {len(hours)}")
    with open(stamp_path, "w", encoding="utf-8") as fh:
        fh.write(manifest_sha + "\n")
    print(f"cache ready {cache_path}")


def perps_config(proto: dict, arm: dict, direction: str) -> dict:
    regime = proto["perps_regime"]
    tiers = {}
    trail = {}
    for label in proto["close_labels"]:
        tiers[label] = proto["tier_ladders"][proto["label_ladder"][label]]
        trail[label] = {"atr_multiple": proto["trail_atr"][label]}
    gate = arm.get("gate_window") or regime["atr_window"]
    strategy = {
        "id": "ra-2017",
        "type": "perps",
        "platform": "hyperliquid",
        "args": ["regime_adaptive", "BTC", "1h", "--mode=paper"],
        "open_strategy": {"name": "regime_adaptive", "params": dict(arm.get("params") or {})},
        "close_strategy": {"name": "trailing_tp_ratchet_regime", "params": {"tp_tiers": tiers}},
        "direction": direction,
        "leverage": 1,
        "capital": proto["capital_usd"],
        "initial_capital": proto["capital_usd"],
        "trailing_stop_atr_mult_regime": {"trend_regime": trail},
        "regime_atr_window": regime["atr_window"],
        "regime_directional_window": regime["directional_window"],
        "regime_gate_window": gate,
        "atr_method": proto["atr_method"],
    }
    if arm.get("allowed_regimes"):
        strategy["allowed_regimes"] = list(arm["allowed_regimes"])
        strategy["regime_gate_on_failure"] = arm["gate_on_failure"]
    return {
        "config_version": 20,
        "regime": {
            "enabled": True,
            "period": regime["period"],
            "adx_threshold": regime["adx_threshold"],
            "windows": regime["windows"],
        },
        "strategies": [strategy],
    }


def audit_candidate(proto: dict, arm: dict) -> dict:
    cand = {
        "name": "regime_adaptive",
        "params": dict(arm.get("params") or {}),
        "type": "spot",
        "direction": "long",
        "atr_method": proto["atr_method"],
        "htf_filter": bool(arm.get("htf_filter")),
    }
    if arm.get("gate_window"):
        spec = proto["perps_regime"]["windows"][arm["gate_window"]]
        cand["regime_enabled"] = True
        cand["allowed_regimes"] = list(arm["allowed_regimes"])
        cand["regime_gate_on_failure"] = arm["gate_on_failure"]
        cand["regime_windows_spec"] = {arm["gate_window"]: spec}
        cand["regime_period"] = int(spec.get("period") or proto["perps_regime"]["period"])
        cand["regime_adx_threshold"] = float(spec.get("adx_threshold") or proto["perps_regime"]["adx_threshold"])
    return cand


_CANDIDATES = {}


def candidate_for(proto: dict, frame: str, direction: str, arm: dict) -> dict:
    key = json.dumps({"frame": frame, "direction": direction, "arm": arm}, sort_keys=True)
    if key in _CANDIDATES:
        return _CANDIDATES[key]
    if frame == "audit":
        cand = audit_candidate(proto, arm)
    else:
        from run_backtest import load_strategy_config
        from tune_live import build_candidate
        cfg = perps_config(proto, arm, direction)
        fd, path = tempfile.mkstemp(prefix="ra2017-", suffix=".json")
        try:
            with os.fdopen(fd, "w", encoding="utf-8") as fh:
                json.dump(cfg, fh)
            resolution = load_strategy_config(path, "ra-2017")
        finally:
            os.remove(path)
        cand = build_candidate("regime_adaptive", resolution["open_strategy"]["params"], resolution)
        cand["htf_filter"] = bool(arm.get("htf_filter"))
        cand["atr_method"] = proto["atr_method"]
        cand["direction"] = direction
    _CANDIDATES[key] = cand
    return cand


def datasets_for(proto: dict, frame: str, arm: dict) -> list:
    fr = proto["frames"][frame]
    intervals = [fr["bar_interval"]] if arm.get("family") == "bar" or arm.get("name") == "bar" else list(fr["intervals"])
    return [(symbol_of(coin, proto), tf) for coin in proto["symbols"] for tf in intervals]


def compare_datasets(proto: dict, frame: str) -> list:
    fr = proto["frames"][frame]
    return [(symbol_of(coin, proto), fr["bar_compare_interval"]) for coin in proto["symbols"]]


def cost_kwargs(proto: dict, kind: str) -> dict:
    costs = proto["costs"]
    if kind == "gross":
        return {"commission_pct": costs["gross_commission_pct"], "slippage_pct": costs["gross_slippage_pct"]}
    if kind == "stress":
        slip = costs["slippage_pct"] * costs["stress_slippage_multiplier"]
        return {"commission_pct": costs["commission_pct"], "slippage_pct": slip}
    if kind == "base":
        return {"commission_pct": costs["commission_pct"], "slippage_pct": costs["slippage_pct"]}
    raise ValueError(kind)


def cluster_id(symbol: str, entry_date) -> str:
    import pandas as pd
    ts = pd.Timestamp(entry_date)
    year, week, _day = ts.isocalendar()
    return f"{symbol}|{int(year)}-W{int(week):02d}"


def compact_leg(leg, symbol: str) -> dict:
    if leg is None:
        return {"error": "no data"}
    positions = ((leg.get("execution") or {}).get("position_list")) or []
    nets, fees, clusters, sides = [], [], [], []
    dropped = 0
    closes = 0
    for pos in positions:
        closes += int(pos.get("closes") or 0)
        notional = float(pos.get("entry_notional") or 0.0)
        if notional <= 0:
            dropped += 1
            continue
        nets.append(round(float(pos.get("net_pnl") or 0.0) / notional * 100.0, 6))
        fees.append(round(float(pos.get("fees") or 0.0) / notional * 100.0, 6))
        sides.append(pos.get("side") or "")
        clusters.append(cluster_id(symbol, pos.get("entry_date")))
    funding = leg.get("funding") if isinstance(leg.get("funding"), dict) else {}
    return {
        "return_pct": leg.get("return_pct"),
        "max_dd_pct": leg.get("max_dd_pct"),
        "ddadj": leg.get("ddadj"),
        "sharpe": leg.get("sharpe"),
        "trades": leg.get("trades"),
        "span_days": leg.get("span_days"),
        "funding_incomplete": bool(leg.get("funding_incomplete")),
        "funding_mode": funding.get("mode"),
        "funding_complete": funding.get("complete"),
        "scored_first_bar": (leg.get("warmup") or {}).get("scored_first_bar"),
        "positions": len(nets),
        "closes": closes,
        "dropped_notional": dropped,
        "net": nets,
        "fee": fees,
        "cluster": clusters,
        "side": sides,
    }


def worker_init(cache_path: str) -> None:
    os.environ[CACHE_ENV] = os.path.abspath(cache_path)
    block_network()


def run_saved(payload: tuple) -> dict:
    task, path, fp = payload
    leg = run_task(task)
    write_json(path, {"fingerprint": fp, "leg": leg})
    return leg


def run_task(task: dict) -> dict:
    proto = protocol()
    import eval_windows as ew
    from registry_loader import load_registry

    frame = task["frame"]
    arm = task["arm"]
    symbol = task["symbol"]
    interval = task["interval"]
    kind = task["kind"]
    window = tuple(proto["windows"][task["window"]])
    t0 = time.time()
    try:
        cand = candidate_for(proto, frame, task["direction"], arm)
        reg = load_registry(proto["frames"][frame]["registry"])
        leg = ew.run_candidate_leg(
            reg, cand, symbol, interval, window, proto["capital_usd"],
            keep_trades=True, funding_mode=proto["funding_mode"],
            warmup_bars=proto["warmup_bars"], intrabar_resolution=proto["intrabar_resolution"],
            **cost_kwargs(proto, kind))
        compact = compact_leg(leg, symbol)
    except Exception as exc:
        compact = {"error": f"{type(exc).__name__}: {exc}"}
    compact.update({
        "frame": frame,
        "direction": task["direction"],
        "window": task["window"],
        "kind": kind,
        "arm": arm["name"],
        "symbol": symbol,
        "interval": interval,
        "seconds": round(time.time() - t0, 3),
        "finished_utc": now_utc(),
    })
    return compact


def task_fingerprint(task: dict, lock: dict) -> str:
    body = {
        "task": task,
        "protocol": lock["protocol_sha256"],
        "engine": lock["engine_digest"],
        "manifest": lock["manifest_sha256"],
    }
    return sha256_bytes(json.dumps(body, sort_keys=True).encode())


def checkpoint_path(work_dir: str, task: dict) -> str:
    arm = task["arm"]["name"]
    symbol = task["symbol"].replace("/", "_")
    name = f"{task['frame']}_{task['direction']}_{task['window']}_{task['kind']}_{arm}_{symbol}_{task['interval']}.json"
    return os.path.join(work_dir, "ckpt", name)


def run_tasks(tasks: list, work_dir: str, cache_path: str, workers: int) -> list:
    lock = require_lock()
    os.makedirs(os.path.join(work_dir, "ckpt"), exist_ok=True)
    pending = []
    done = []
    for task in tasks:
        path = checkpoint_path(work_dir, task)
        fp = task_fingerprint(task, lock)
        if os.path.exists(path):
            rec = load_json(path)
            if rec.get("fingerprint") == fp:
                done.append(rec["leg"])
                continue
        pending.append((task, path, fp))
    print(f"tasks {len(tasks)} cached {len(done)} pending {len(pending)}", flush=True)
    if pending:
        ctx = multiprocessing.get_context("spawn")
        procs = min(workers, len(pending))
        with ctx.Pool(processes=procs, initializer=worker_init, initargs=(cache_path,)) as pool:
            for i, leg in enumerate(pool.imap_unordered(run_saved, pending), 1):
                print(f"scored {i}/{len(pending)} {leg.get('frame')} {leg.get('direction')} {leg.get('arm')} "
                      f"{leg.get('symbol')} {leg.get('interval')} {leg.get('window')} {leg.get('kind')} "
                      f"{leg.get('seconds')}s", flush=True)
    out = []
    for task in tasks:
        path = checkpoint_path(work_dir, task)
        fp = task_fingerprint(task, lock)
        rec = load_json(path)
        if rec.get("fingerprint") != fp:
            raise SystemExit(f"checkpoint fingerprint mismatch for {path}")
        out.append(rec["leg"])
    return out


def make_tasks(proto: dict, frame: str, direction: str, arms: list, windows: list, kinds: list) -> list:
    tasks = []
    for arm in arms:
        for symbol, interval in datasets_for(proto, frame, arm):
            for window in windows:
                for kind in kinds:
                    tasks.append({
                        "frame": frame,
                        "direction": direction,
                        "window": window,
                        "kind": kind,
                        "symbol": symbol,
                        "interval": interval,
                        "arm": arm,
                    })
    return tasks


def legs_for(rows: list, frame: str, direction: str, arm: str, window: str, kind: str,
             datasets: list | None = None) -> list:
    want = {(s, tf) for s, tf in datasets} if datasets is not None else None
    out = []
    for row in rows:
        if row.get("frame") != frame or row.get("direction") != direction:
            continue
        if row.get("arm") != arm or row.get("window") != window or row.get("kind") != kind:
            continue
        if want is not None and (row.get("symbol"), row.get("interval")) not in want:
            continue
        out.append(row)
    return out


def pooled(rows: list) -> dict:
    nets, fees, clusters, sides = [], [], [], []
    closes = 0
    errors = []
    incomplete = False
    for row in rows:
        if row.get("error"):
            errors.append(row["error"])
        if row.get("funding_incomplete") or row.get("funding_mode") not in (None, "charge"):
            if row.get("funding_mode") != "charge" or row.get("funding_incomplete"):
                incomplete = True
        nets.extend(row.get("net") or [])
        fees.extend(row.get("fee") or [])
        clusters.extend(row.get("cluster") or [])
        sides.extend(row.get("side") or [])
        closes += int(row.get("closes") or 0)
    return {
        "positions": len(nets),
        "closes": closes,
        "clusters": len(set(clusters)),
        "nets": nets,
        "fees": fees,
        "cluster_ids": clusters,
        "long": sum(1 for s in sides if s == "long"),
        "short": sum(1 for s in sides if s == "short"),
        "errors": errors,
        "funding_incomplete": incomplete or any(r.get("funding_incomplete") for r in rows),
        "mean_net": statistics.fmean(nets) if nets else None,
        "mean_fee": statistics.fmean(fees) if fees else None,
        "mean_gross": statistics.fmean(n + f for n, f in zip(nets, fees)) if nets else None,
    }


def mean_key(rows: list, key: str):
    vals = [r[key] for r in rows if r.get(key) is not None and not r.get("error")]
    if len(vals) != len(rows) or not vals:
        return None
    return statistics.fmean(vals)


def eligible(rows: list, proto: dict, datasets: list) -> dict:
    stats = proto["statistics"]
    if len(rows) != len(datasets):
        return {"ok": False, "reason": "missing dataset"}
    if any(r.get("error") for r in rows):
        return {"ok": False, "reason": "leg error"}
    if any(r.get("funding_incomplete") or r.get("funding_complete") is False or r.get("funding_mode") != "charge"
           for r in rows):
        return {"ok": False, "reason": "funding incomplete"}
    pool = pooled(rows)
    if pool["positions"] < stats["min_positions"]:
        return {"ok": False, "reason": "position floor", **{k: pool[k] for k in ("positions", "clusters")}}
    if pool["clusters"] < stats["min_clusters"]:
        return {"ok": False, "reason": "cluster floor", **{k: pool[k] for k in ("positions", "clusters")}}
    return {"ok": True, "reason": "ok", "positions": pool["positions"], "clusters": pool["clusters"]}


def arm_summary(rows: list, proto: dict, frame: str, direction: str, arm: dict, window: str) -> dict:
    data = datasets_for(proto, frame, arm)
    got = legs_for(rows, frame, direction, arm["name"], window, "base", data)
    gate = eligible(got, proto, data)
    return {
        "arm": arm["name"],
        "family": arm.get("family"),
        "score": mean_key(got, "ddadj"),
        "mean_return_pct": mean_key(got, "return_pct"),
        "mean_max_dd_pct": mean_key(got, "max_dd_pct"),
        "eligible": gate["ok"],
        "reason": gate["reason"],
        "positions": gate.get("positions"),
        "clusters": gate.get("clusters"),
        "datasets": len(got),
    }


def plateau_ok(summaries: dict, winner: str, proto: dict) -> bool:
    grid = proto["statistics"]["z_grid"]
    default = proto["statistics"]["z_default"]
    ratio = proto["statistics"]["plateau_ratio"]
    by_z = {default: "control"}
    for arm in proto["arms"]:
        if arm.get("family") == "threshold":
            by_z[arm["z"]] = arm["name"]
    z_of = {name: z for z, name in by_z.items()}
    z = z_of[winner]
    neighbors = []
    if z != grid[0]:
        neighbors.append(grid[grid.index(z) - 1])
    if z != grid[-1]:
        neighbors.append(grid[grid.index(z) + 1])
    scores = []
    for nz in neighbors:
        name = by_z[nz]
        score = summaries[name]["score"]
        if score is None:
            return False
        scores.append(score)
    best_neighbor = max(scores)
    winner_score = summaries[winner]["score"]
    if winner_score <= best_neighbor:
        return False
    if winner_score > 0 and best_neighbor < ratio * winner_score:
        return False
    return True


def pick_family(summaries: dict, proto: dict, family: str, control_score: float) -> str | None:
    names = [a["name"] for a in proto["arms"] if a.get("family") == family]
    ranked = []
    for name in names:
        row = summaries[name]
        if not row["eligible"] or row["score"] is None:
            continue
        if row["score"] <= control_score:
            continue
        ranked.append((-row["score"], -(row["positions"] or 0), name))
    if not ranked:
        return None
    ranked.sort()
    winner = ranked[0][2]
    if family == "threshold" and not plateau_ok(summaries, winner, proto):
        return None
    return winner


def pick_bar(rows: list, proto: dict, frame: str, direction: str, control_rows: list) -> str | None:
    arm = arm_by_name(proto, "bar")
    data = datasets_for(proto, frame, arm)
    got = legs_for(rows, frame, direction, "bar", "train", "base", data)
    gate = eligible(got, proto, data)
    compare = compare_datasets(proto, frame)
    base = legs_for(control_rows, frame, direction, "control", "train", "base", compare)
    base_gate = eligible(base, proto, compare)
    score = mean_key(got, "ddadj")
    base_score = mean_key(base, "ddadj")
    if not gate["ok"] or not base_gate["ok"] or score is None or base_score is None:
        return None
    if score <= 0 or score <= base_score:
        return None
    return "bar"


def combo_from_picks(proto: dict, picks: dict) -> dict | None:
    chosen = [picks[name] for name in FILTER_FAMILIES if picks.get(name)]
    if len(chosen) < 2:
        return None
    arm = {"name": "combo", "family": "combo", "search": True, "params": {}}
    for name in chosen:
        src = arm_by_name(proto, name)
        if src.get("gate_window"):
            arm["gate_window"] = src["gate_window"]
            arm["allowed_regimes"] = list(src["allowed_regimes"])
            arm["gate_on_failure"] = src["gate_on_failure"]
        if src.get("htf_filter"):
            arm["htf_filter"] = True
        if src.get("params"):
            arm["params"].update(src["params"])
    return arm


def suggestion_name(picks: dict) -> str | None:
    if picks.get("combo"):
        return "combo"
    filters = [picks[name] for name in FILTER_FAMILIES if picks.get(name)]
    if len(filters) == 1:
        return filters[0]
    if not filters and picks.get("bar"):
        return picks["bar"]
    return None


def select_cmd(cache_path: str, work_dir: str, workers: int) -> None:
    if os.path.exists(SELECTION_PATH):
        raise SystemExit("selection.json already exists")
    if os.path.exists(RESULTS_PATH):
        raise SystemExit("results.json already exists")
    build_cache(cache_path)
    proto = protocol()
    tasks = []
    for frame, fr in proto["frames"].items():
        for direction in fr["directions"]:
            tasks.extend(make_tasks(proto, frame, direction, proto["arms"], ["train"], ["base"]))
    rows = run_tasks(tasks, work_dir, cache_path, workers)
    directions = {}
    family = []
    for frame, fr in proto["frames"].items():
        directions[frame] = {}
        for direction in fr["directions"]:
            summaries = {}
            for arm in proto["arms"]:
                summaries[arm["name"]] = arm_summary(rows, proto, frame, direction, arm, "train")
                if arm.get("search"):
                    family.append({"frame": frame, "direction": direction, "arm": arm["name"]})
            control = summaries["control"]
            control_score = control["score"] if control["eligible"] and control["score"] is not None else math.inf
            picks = {
                "gate": pick_family(summaries, proto, "gate", control_score) if control["eligible"] else None,
                "htf": pick_family(summaries, proto, "htf", control_score) if control["eligible"] else None,
                "threshold": pick_family(summaries, proto, "threshold", control_score) if control["eligible"] else None,
                "bar": pick_bar(rows, proto, frame, direction, rows) if control["eligible"] else None,
            }
            combo = combo_from_picks(proto, picks)
            combo_summary = None
            if combo is not None:
                combo_rows = run_tasks(
                    make_tasks(proto, frame, direction, [combo], ["train"], ["base"]),
                    work_dir, cache_path, workers)
                rows.extend(combo_rows)
                combo_summary = arm_summary(combo_rows, proto, frame, direction, combo, "train")
                picks["combo"] = "combo"
                family.append({"frame": frame, "direction": direction, "arm": "combo"})
            else:
                picks["combo"] = None
            shortlist = ["control"]
            for key in ("gate", "htf", "threshold", "bar", "combo"):
                if picks.get(key) and picks[key] not in shortlist:
                    shortlist.append(picks[key])
            directions[frame][direction] = {
                "arms": summaries,
                "combo": combo,
                "combo_train": combo_summary,
                "picks": picks,
                "suggestion_arm": suggestion_name(picks),
                "shortlist": shortlist,
                "control_eligible": control["eligible"],
            }
    doc = {
        "selected_at": now_utc(),
        "protocol_sha256": protocol_sha256(),
        "directions": directions,
        "family": family,
        "family_size": len(family),
    }
    write_json(SELECTION_PATH, doc)
    for frame, dirs in directions.items():
        for direction, block in dirs.items():
            print(f"select {frame} {direction} suggestion={block['suggestion_arm']} shortlist={block['shortlist']}")


def shortlist_arms(proto: dict, selection: dict, frame: str, direction: str) -> list:
    block = selection["directions"][frame][direction]
    arms = []
    for name in block["shortlist"]:
        if name == "combo":
            arms.append(block["combo"])
        else:
            arms.append(arm_by_name(proto, name))
    return arms


def judge_cmd(cache_path: str, work_dir: str, workers: int) -> None:
    if not os.path.exists(SELECTION_PATH):
        raise SystemExit("selection.json is missing; run select first")
    if not os.path.exists(COVERAGE_PATH):
        raise SystemExit("coverage.json is missing; run coverage first")
    if os.path.exists(RESULTS_PATH):
        raise SystemExit("results.json already exists")
    build_cache(cache_path)
    proto = protocol()
    selection = load_json(SELECTION_PATH)
    if selection.get("protocol_sha256") != protocol_sha256():
        raise SystemExit("selection.json was built for a different protocol hash")
    tasks = []
    windows = list(proto["judgment_windows"]) + ["continuous"]
    for frame, fr in proto["frames"].items():
        for direction in fr["directions"]:
            arms = shortlist_arms(proto, selection, frame, direction)
            tasks.extend(make_tasks(proto, frame, direction, arms, windows, ["gross", "base", "stress"]))
    rows = run_tasks(tasks, work_dir, cache_path, workers)
    m1 = m1_cmd(proto, selection, cache_path, work_dir, workers)
    first = min((r.get("finished_utc") or "" for r in rows), default="")
    doc = {
        "judged_at": now_utc(),
        "first_judged_utc": first,
        "protocol_sha256": protocol_sha256(),
        "selection_sha256": sha256_file(SELECTION_PATH),
        "rows": rows,
        "m1": m1,
    }
    doc["verdicts"] = decide(proto, selection, doc)
    write_json(RESULTS_PATH, doc)
    for frame, dirs in doc["verdicts"]["frames"].items():
        for direction, verdict in dirs.items():
            print(f"verdict {frame} {direction} {verdict['verdict']} {verdict['reason']}")


def m1_task(task: dict) -> dict:
    proto = protocol()
    import eval_windows as ew
    from registry_loader import load_registry

    t0 = time.time()
    window = tuple(proto["windows"][task["window"]])
    datasets = [tuple(pair) for pair in task["datasets"]]
    try:
        reg = load_registry(proto["frames"][task["frame"]]["registry"])
        directed = {}
        for symbol, timeframe in datasets:
            key = ew.dataset_key(symbol, timeframe)
            directed[key] = {}
            for name in ew.INCUMBENTS:
                directed[key][name] = ew.run_leg(
                    reg, name, None, symbol, timeframe, window, capital=proto["capital_usd"],
                    direction=task["direction"], funding_mode=proto["funding_mode"],
                    warmup_bars=proto["warmup_bars"],
                    intrabar_resolution=proto["intrabar_resolution"],
                    **cost_kwargs(proto, "base"))
        bars = ew.incumbent_bars(directed)
        score = {"window": task["window"], "direction": task["direction"], "frame": task["frame"],
                 "datasets": task["datasets"], "bars": {
                     ds: None if bar is None else {"sharpe": bar["sharpe"], "ddadj": bar["ddadj"], "n": bar["n"]}
                     for ds, bar in bars.items()
                 }, "seconds": round(time.time() - t0, 3)}
        return score
    except Exception as exc:
        return {"frame": task["frame"], "direction": task["direction"], "window": task["window"],
                "error": f"{type(exc).__name__}: {exc}", "seconds": round(time.time() - t0, 3)}


def m1_cmd(proto: dict, selection: dict, cache_path: str, work_dir: str, workers: int) -> dict:
    tasks = []
    for frame, fr in proto["frames"].items():
        for direction in proto["m1_directions"]:
            if direction not in fr["directions"]:
                continue
            datasets = []
            for arm in shortlist_arms(proto, selection, frame, direction):
                for pair in datasets_for(proto, frame, arm):
                    if list(pair) not in datasets:
                        datasets.append(list(pair))
            for window in proto["judgment_windows"]:
                tasks.append({"frame": frame, "direction": direction, "window": window, "datasets": datasets})
    if not tasks:
        return {}
    lock = require_lock()
    os.makedirs(os.path.join(work_dir, "ckpt"), exist_ok=True)
    pending = []
    done = []
    for task in tasks:
        fp = sha256_bytes(json.dumps({"m1": task, "protocol": lock["protocol_sha256"],
                                       "engine": lock["engine_digest"]}, sort_keys=True).encode())
        path = os.path.join(work_dir, "ckpt", f"m1_{task['frame']}_{task['direction']}_{task['window']}.json")
        if os.path.exists(path) and load_json(path).get("fingerprint") == fp:
            done.append(load_json(path)["row"])
        else:
            pending.append((task, path, fp))
    if pending:
        ctx = multiprocessing.get_context("spawn")
        with ctx.Pool(processes=min(workers, len(pending)), initializer=worker_init,
                      initargs=(cache_path,)) as pool:
            scored = pool.map(m1_task, [task for task, _p, _f in pending], chunksize=1)
        for (task, path, fp), row in zip(pending, scored):
            write_json(path, {"fingerprint": fp, "row": row})
            done.append(row)
            print(f"m1 {row.get('frame')} {row.get('direction')} {row.get('window')} {row.get('seconds')}s",
                  flush=True)
    return {"rows": done}


def window_years(proto: dict, names: list) -> float:
    import pandas as pd
    total = 0.0
    for name in names:
        start, end = proto["windows"][name]
        total += (pd.Timestamp(end) - pd.Timestamp(start)).days / 365.25
    return total


def measurement(rows: list, proto: dict, frame: str, direction: str, arm: dict, window: str, kind: str) -> dict:
    data = datasets_for(proto, frame, arm)
    got = legs_for(rows, frame, direction, arm["name"], window, kind, data)
    pool = pooled(got)
    return {
        "datasets": len(got),
        "expected": len(data),
        "mean_return_pct": mean_key(got, "return_pct"),
        "mean_max_dd_pct": mean_key(got, "max_dd_pct"),
        "worst_dd_pct": min((r["max_dd_pct"] for r in got if r.get("max_dd_pct") is not None), default=None),
        "eligible_inputs": not any(r.get("error") for r in got) and len(got) == len(data),
        "leg_returns": [r.get("return_pct") for r in got],
        **{k: pool[k] for k in ("positions", "closes", "clusters", "mean_net", "mean_fee", "mean_gross",
                                "funding_incomplete", "errors", "long", "short", "nets", "fees", "cluster_ids")},
    }


def sign_flip(nets: list, clusters: list, proto: dict) -> dict:
    from gross_edge_noise import cluster_sign_flip_permutation
    stats = proto["statistics"]
    return cluster_sign_flip_permutation(nets, clusters, n_resamples=stats["resamples"], seed=stats["seed"])


def perps_decision_context(proto: dict) -> dict:
    coverage = load_json(COVERAGE_PATH) if os.path.exists(COVERAGE_PATH) else {}
    native = bool(coverage.get("native_covers_judgment"))
    lot = coverage.get("execution_spec") is not None
    ok = native and lot
    reason = "native candles cover the judgment windows and a lot spec is present" if ok else (
        "native Hyperliquid candles do not cover every frozen judgment window"
        if not native else "the scored book has no venue lot or minimum spec")
    if not native and not lot:
        reason = ("native Hyperliquid candles do not cover every frozen judgment window, "
                  "and the scored book has no venue lot or minimum spec")
    return {"ok": ok, "reason": reason, "native_covers_judgment": native, "execution_spec": lot}


def heldout_bundle(rows: list, proto: dict, frame: str, direction: str, arm: dict, kind: str) -> dict:
    parts = [measurement(rows, proto, frame, direction, arm, window, kind) for window in proto["judgment_windows"]]
    nets, clusters, fees = [], [], []
    errors = []
    incomplete = False
    returns = []
    positions = closes = 0
    for part in parts:
        nets.extend(part["nets"])
        fees.extend(part["fees"])
        clusters.extend(part["cluster_ids"])
        errors.extend(part["errors"])
        incomplete = incomplete or part["funding_incomplete"] or not part["eligible_inputs"]
        if any(v is None for v in part["leg_returns"]) or not part["leg_returns"]:
            incomplete = True
        else:
            returns.extend(part["leg_returns"])
        positions += part["positions"]
        closes += part["closes"]
    return {
        "positions": positions,
        "closes": closes,
        "clusters": len(set(clusters)),
        "nets": nets,
        "cluster_ids": clusters,
        "mean_net": statistics.fmean(nets) if nets else None,
        "mean_fee": statistics.fmean(fees) if fees else None,
        "mean_leg_return_pct": None if incomplete or not returns else statistics.fmean(returns),
        "funding_incomplete": incomplete,
        "errors": errors,
        "parts": [{k: v for k, v in part.items() if k not in ("nets", "fees", "cluster_ids", "leg_returns")} for part in parts],
    }


def arm_obj(selection: dict, proto: dict, frame: str, direction: str, name: str) -> dict:
    block = selection["directions"][frame][direction]
    if name == "combo":
        return block["combo"]
    return arm_by_name(proto, name)


def decide(proto: dict, selection: dict, results: dict) -> dict:
    from auto_suggest import apply_family_correction

    rows = results.get("rows") or []
    perps_ctx = perps_decision_context(proto)
    tests = []
    for member in selection["family"]:
        frame, direction, name = member["frame"], member["direction"], member["arm"]
        key = f"{frame}|{direction}|{name}"
        block = selection["directions"][frame][direction]
        judged = name in block["shortlist"]
        p_value = 1.0
        sample = None
        if judged:
            arm = arm_obj(selection, proto, frame, direction, name)
            sample = heldout_bundle(rows, proto, frame, direction, arm, "base")
            floor = (sample["positions"] >= proto["statistics"]["min_positions"]
                     and sample["clusters"] >= proto["statistics"]["min_clusters"]
                     and not sample["funding_incomplete"] and not sample["errors"])
            if floor and sample["nets"]:
                flipped = sign_flip(sample["nets"], sample["cluster_ids"], proto)
                p_value = float(flipped["p_value"])
                sample = {**sample, "p_value": p_value, "flip": {k: flipped[k] for k in ("n", "clusters", "mean", "p_value")}}
            else:
                sample = {**sample, "p_value": None}
        tests.append({"candidate_key": key, "p": p_value, "judged": judged})
    family_tests = [{"candidate_key": t["candidate_key"], "p": t["p"]} for t in tests]
    correction = apply_family_correction(family_tests, alpha=proto["statistics"]["alpha"])
    bh_pass = {t["candidate_key"]: bool(t["bh_pass"]) for t in family_tests}

    frames = {}
    suggestions = []
    for frame, fr in proto["frames"].items():
        frames[frame] = {}
        for direction in fr["directions"]:
            block = selection["directions"][frame][direction]
            name = block["suggestion_arm"]
            context_ok = True
            context_reason = "historical audit path"
            if frame == "perps":
                context_ok = perps_ctx["ok"]
                context_reason = perps_ctx["reason"]
            proxy = evaluate_suggestion(proto, rows, frame, direction, name, block, bh_pass, context_ok=True)
            if not context_ok:
                verdict = {"verdict": "inconclusive", "reason": context_reason, "suggestion_arm": name,
                           "proxy_measurement": proxy["verdict"], "proxy_reason": proxy["reason"]}
            else:
                verdict = proxy
            frames[frame][direction] = verdict
            if verdict["verdict"] == "positive" and name:
                suggestions.append({"frame": frame, "direction": direction, "arm": name})
    return {"frames": frames, "correction": {k: correction[k] for k in correction},
            "family_tests": family_tests, "suggestions": suggestions, "details_omitted": True}


def evaluate_suggestion(proto, rows, frame, direction, name, block, bh_pass, context_ok: bool) -> dict:
    if not context_ok:
        return {"verdict": "inconclusive", "reason": "decision context", "suggestion_arm": name}
    if not block.get("control_eligible"):
        return {"verdict": "inconclusive", "reason": "training control missed the sample or funding floor",
                "suggestion_arm": name}
    if not name:
        control = arm_by_name(proto, "control")
        sample = heldout_bundle(rows, proto, frame, direction, control, "base")
        if sample["funding_incomplete"] or sample["errors"]:
            return {"verdict": "inconclusive", "reason": "baseline judgment evidence is incomplete",
                    "suggestion_arm": None}
        return {"verdict": "negative", "reason": "no filter cleared the training rule", "suggestion_arm": None}
    arm = block["combo"] if name == "combo" else arm_by_name(proto, name)
    base = heldout_bundle(rows, proto, frame, direction, arm, "base")
    stress = heldout_bundle(rows, proto, frame, direction, arm, "stress")
    floor = proto["statistics"]
    if base["funding_incomplete"] or stress["funding_incomplete"] or base["errors"] or stress["errors"]:
        return {"verdict": "inconclusive", "reason": "suggestion arm evidence is incomplete", "suggestion_arm": name}
    if (base["positions"] < floor["min_positions"] or base["clusters"] < floor["min_clusters"]
            or stress["positions"] < floor["min_positions"]):
        return {"verdict": "inconclusive", "reason": "suggestion arm is below the held-out sample floor",
                "suggestion_arm": name}
    returns_ok = (
        base["mean_net"] is not None and base["mean_net"] > 0
        and stress["mean_net"] is not None and stress["mean_net"] > 0
        and base["mean_leg_return_pct"] is not None and base["mean_leg_return_pct"] > 0
        and stress["mean_leg_return_pct"] is not None and stress["mean_leg_return_pct"] > 0)
    key = f"{frame}|{direction}|{name}"
    stats_ok = bool(bh_pass.get(key))
    if not returns_ok or not stats_ok:
        why = "held-out net return is not positive at base and stress" if not returns_ok else "the family correction does not pass"
        return {"verdict": "negative", "reason": why, "suggestion_arm": name,
                "mean_net_base": base["mean_net"], "mean_net_stress": stress["mean_net"],
                "mean_leg_base": base["mean_leg_return_pct"], "mean_leg_stress": stress["mean_leg_return_pct"],
                "bh_pass": stats_ok}
    return {"verdict": "positive", "reason": "sample, return and correction gates pass", "suggestion_arm": name,
            "mean_net_base": base["mean_net"], "mean_net_stress": stress["mean_net"],
            "mean_leg_base": base["mean_leg_return_pct"], "mean_leg_stress": stress["mean_leg_return_pct"],
            "bh_pass": True}


def continuous_dd(rows: list, proto: dict, frame: str, direction: str, arm: dict) -> dict:
    data = datasets_for(proto, frame, arm)
    got = legs_for(rows, frame, direction, arm["name"], "continuous", "base", data)
    worst = min((r["max_dd_pct"] for r in got if r.get("max_dd_pct") is not None), default=None)
    return {"worst_dd_pct": worst, "datasets": len(got), "expected": len(data)}


def render_cmd() -> None:
    require_lock()
    if not os.path.exists(RESULTS_PATH) or not os.path.exists(SELECTION_PATH):
        raise SystemExit("render needs selection.json and results.json")
    proto = protocol()
    selection = load_json(SELECTION_PATH)
    results = load_json(RESULTS_PATH)
    verdicts = decide(proto, selection, results)
    for frame, dirs in verdicts["frames"].items():
        for direction, row in dirs.items():
            stored = results["verdicts"]["frames"][frame][direction]
            if (row["verdict"], row["reason"], row.get("suggestion_arm")) != (
                    stored["verdict"], stored["reason"], stored.get("suggestion_arm")):
                raise SystemExit(f"recomputed verdict differs for {frame} {direction}")
    years = window_years(proto, proto["judgment_windows"])
    lines = [
        "# regime_adaptive entry filters",
        "",
        "Issue 2017. This study asks whether an existing entry filter keeps a positive return after fees and funding.",
        "The protocol was locked before the judgment windows were scored. The shortlist was written before those windows were scored.",
        "",
        f"Protocol `{results['protocol_sha256']}`. Lock file `protocol_lock.json`. Selection `{results['selection_sha256']}`.",
        "",
        "## Verdicts",
        "",
        "| Frame | Direction | Verdict | Suggestion arm | Reason |",
        "| --- | --- | --- | --- | --- |",
    ]
    for frame, dirs in verdicts["frames"].items():
        for direction, row in dirs.items():
            lines.append(f"| {frame} | {direction} | {row['verdict']} | {row.get('suggestion_arm')} | {row['reason']} |")
    lines += ["", "## What the frames are", ""]
    lines.append(proto["frames"]["audit"]["text"])
    lines.append("")
    lines.append(proto["frames"]["perps"]["text"])
    lines += ["", "## Windows", ""]
    lines.append(proto["window_rules"]["disjoint"])
    lines.append("")
    lines.append(proto["window_rules"]["prior_reads"])
    lines.append("")
    lines.append(proto["window_rules"]["judge_2023_start"])
    lines += ["", "## Training and the shortlist", ""]
    for frame, dirs in selection["directions"].items():
        for direction, block in dirs.items():
            lines.append(f"### {frame} {direction}")
            lines.append("")
            lines.append("| Arm | Family | Train score | Positions | Clusters | Eligible | Status |")
            lines.append("| --- | --- | --- | --- | --- | --- | --- |")
            picked = {v for v in block["picks"].values() if v}
            for name, row in block["arms"].items():
                status = "shortlist" if name in block["shortlist"] else "unjudged"
                if name in picked:
                    status = "pick"
                lines.append(
                    f"| {name} | {row['family']} | {fmt(row['score'])} | {row['positions']} | {row['clusters']} | {row['eligible']} | {status} |")
            if block.get("combo_train"):
                row = block["combo_train"]
                lines.append(
                    f"| combo | combo | {fmt(row['score'])} | {row['positions']} | {row['clusters']} | {row['eligible']} | shortlist |")
            lines.append("")
            lines.append(f"Suggestion arm: `{block['suggestion_arm']}`.")
            lines.append("")
    lines += ["## Judged shortlist", ""]
    lines.append("Position means are not annualized. Positions per year use the sum of the judgment-window lengths, "
                 f"{years:.3f} years. Fee drag is the position fee divided by entry notional. "
                 "Gross position return is the net return plus that drag, so it is after funding.")
    lines.append("")
    rows = results["rows"]
    for frame, dirs in selection["directions"].items():
        for direction, block in dirs.items():
            lines.append(f"### {frame} {direction}")
            lines.append("")
            lines.append("| Arm | Positions | Closes | Clusters | Pos/year | Net % | Gross % | Fee drag pp | Leg return % | Stress net % | Stress leg % | Worst continuous DD % |")
            lines.append("| --- | --- | --- | --- | --- | --- | --- | --- | --- | --- | --- | --- |")
            for name in block["shortlist"]:
                arm = block["combo"] if name == "combo" else arm_by_name(proto, name)
                base = heldout_bundle(rows, proto, frame, direction, arm, "base")
                gross = heldout_bundle(rows, proto, frame, direction, arm, "gross")
                stress = heldout_bundle(rows, proto, frame, direction, arm, "stress")
                cont = continuous_dd(rows, proto, frame, direction, arm)
                per_year = (base["positions"] / years) if years else None
                fee = base.get("mean_fee")
                lines.append(
                    f"| {name} | {base['positions']} | {base['closes']} | {base['clusters']} | {fmt(per_year)} | "
                    f"{fmt(base['mean_net'])} | {fmt(gross['mean_net'])} | {fmt(fee)} | {fmt(base['mean_leg_return_pct'])} | "
                    f"{fmt(stress['mean_net'])} | {fmt(stress['mean_leg_return_pct'])} | {fmt(cont['worst_dd_pct'])} |")
            lines.append("")
            if frame == "perps":
                lines.append(f"Proxy measurement: {verdicts['frames'][frame][direction].get('proxy_measurement')} "
                             f"({verdicts['frames'][frame][direction].get('proxy_reason')}).")
                lines.append("That label is not the frame verdict.")
                lines.append("")
    lines += ["## Statistics", ""]
    corr = verdicts["correction"]
    lines.append(f"Family size {corr['m']}. Method {corr['method']}. Alpha {corr['alpha']}. "
                 f"Survivors {corr['n_survivors']}.")
    lines.append("Tests use base-cost net position returns on the judgment windows. "
                 "Unjudged searched arms enter with p=1. Stress cost is a robustness check and is not an extra hypothesis.")
    lines.append("")
    lines.append("| Hypothesis | p | BH pass |")
    lines.append("| --- | --- | --- |")
    for test in verdicts["family_tests"]:
        lines.append(f"| {test['candidate_key']} | {fmt(test['p'], 6)} | {test['bh_pass']} |")
    lines += ["", "## M1 incumbent comparison", ""]
    lines.append(proto["verdict"]["m1"])
    lines.append("")
    lines.append("A window passes when the arm's mean Sharpe and mean drawdown-adjusted return are both above the incumbent bar, at least half the datasets trade, and funding is complete. This pass is not the profitability verdict.")
    lines.append("")
    lines.extend(m1_report_lines(proto, selection, results))
    lines += ["", "## Costs and close", "", proto["costs"]["text"], ""]
    lines.append("Proxy funding uses the pinned tema_cross_2016 manifest. Hours that manifest marks venue-absent are charged at zero. They are not missing data.")
    lines += ["", proto["close_source"], "", proto["perps_regime"]["text"], ""]
    lines += ["## Reproduce", "",
              "```",
              "uv run --no-sync python backtest/candidates/regime_adaptive_2017/run_study.py verify-inputs",
              "uv run --no-sync python backtest/candidates/regime_adaptive_2017/run_study.py lock",
              "uv run --no-sync python backtest/candidates/regime_adaptive_2017/run_study.py coverage",
              "uv run --no-sync python backtest/candidates/regime_adaptive_2017/run_study.py select --cache /tmp/ra2017.db --work /tmp/ra2017",
              "uv run --no-sync python backtest/candidates/regime_adaptive_2017/run_study.py judge --cache /tmp/ra2017.db --work /tmp/ra2017",
              "uv run --no-sync python backtest/candidates/regime_adaptive_2017/run_study.py render",
              "```",
              "",
              "No command sends an order. This study does not change a live config, a paper config, a registry default, the quarantine roster, or edge_status.",
              ""]
    if verdicts["suggestions"]:
        lines.append("Suggested configs are written only for a positive verdict.")
    else:
        lines.append("No frame and direction is positive, so this study writes no suggested config.")
    lines.append("")
    write_text(REPORT_PATH, "\n".join(lines) + "\n")
    if os.path.exists(SUGGESTED_PATH) and not verdicts["suggestions"]:
        os.remove(SUGGESTED_PATH)
    for item in verdicts["suggestions"]:
        arm = arm_obj(selection, proto, item["frame"], item["direction"], item["arm"])
        write_json(SUGGESTED_PATH, {
            "frame": item["frame"],
            "direction": item["direction"],
            "arm": arm,
            "note": "Offline research output. It is not a live or paper promotion.",
        })
    print(f"wrote {REPORT_PATH}")


def m1_report_lines(proto: dict, selection: dict, results: dict) -> list:
    bars = {}
    lines = []
    for row in (results.get("m1") or {}).get("rows") or []:
        if row.get("error"):
            lines.append(f"- {row.get('frame')} {row.get('direction')} {row.get('window')}: {row['error']}")
            continue
        bars[(row["frame"], row["direction"], row["window"])] = row.get("bars") or {}
    for frame, dirs in selection["directions"].items():
        for direction, block in dirs.items():
            if direction not in proto["m1_directions"]:
                lines.append(f"- {frame} {direction}: no incumbent comparison.")
                continue
            for name in block["shortlist"]:
                arm = block["combo"] if name == "combo" else arm_by_name(proto, name)
                for window in proto["judgment_windows"]:
                    got = legs_for(results["rows"], frame, direction, name, window, "base",
                                   datasets_for(proto, frame, arm))
                    barset = bars.get((frame, direction, window)) or {}
                    sharpes, ddadjs, bsharpes, bdds = [], [], [], []
                    for leg in got:
                        bar = barset.get(f"{leg.get('symbol')} {leg.get('interval')}")
                        if leg.get("error") or not bar or leg.get("sharpe") is None or leg.get("ddadj") is None:
                            continue
                        sharpes.append(leg["sharpe"])
                        ddadjs.append(leg["ddadj"])
                        bsharpes.append(bar["sharpe"])
                        bdds.append(bar["ddadj"])
                    traded = sum(1 for leg in got if (leg.get("trades") or 0) > 0)
                    if any(leg.get("funding_incomplete") for leg in got) or len(sharpes) != len(got) or not got:
                        status = "incomplete"
                    elif traded < math.ceil(len(got) / 2):
                        status = "degenerate"
                    elif statistics.fmean(sharpes) > statistics.fmean(bsharpes) and statistics.fmean(ddadjs) > statistics.fmean(bdds):
                        status = "pass"
                    else:
                        status = "fail"
                    lines.append(f"- {frame} {direction} {name} {window}: {status}")
    return lines


def write_text(path: str, text: str) -> None:
    tmp = path + ".tmp"
    with open(tmp, "w", encoding="utf-8") as fh:
        fh.write(text)
    os.replace(tmp, path)


def fmt(value, digits=4):
    if value is None:
        return "-"
    if isinstance(value, bool):
        return str(value)
    try:
        if isinstance(value, float) and (math.isnan(value) or math.isinf(value)):
            return "-"
        return f"{float(value):.{digits}f}"
    except (TypeError, ValueError):
        return str(value)


def smoke_cmd(cache_path: str, work_dir: str) -> None:
    """Build every candidate and score two training legs. Does not write selection."""
    build_cache(cache_path)
    proto = protocol()
    os.environ[CACHE_ENV] = os.path.abspath(cache_path)
    block_network()
    built = 0
    for frame, fr in proto["frames"].items():
        for direction in fr["directions"]:
            for arm in proto["arms"]:
                cand = candidate_for(proto, frame, direction, arm)
                if cand.get("name") != "regime_adaptive":
                    raise SystemExit(f"bad candidate {frame} {arm['name']}")
                if frame == "perps" and not cand.get("close_strategies"):
                    raise SystemExit(f"perps {arm['name']} has no close")
                if frame == "perps" and not (cand.get("stop_context") or {}).get("trailing_stop_atr_mult_regime"):
                    raise SystemExit(f"perps {arm['name']} has no regime trail")
                built += 1
    print(f"built {built} candidates")
    tasks = [
        make_tasks(proto, "audit", "long", [arm_by_name(proto, "control")], ["train"], ["base"])[0],
        make_tasks(proto, "perps", "long", [arm_by_name(proto, "control")], ["train"], ["base"])[0],
    ]
    for task in tasks:
        leg = run_task(task)
        if leg.get("error"):
            raise SystemExit(leg["error"])
        print(f"smoke {leg['frame']} {leg['symbol']} {leg['interval']} trades={leg.get('trades')} "
              f"funding={leg.get('funding_mode')} seconds={leg.get('seconds')}")


def main(argv=None) -> int:
    parser = argparse.ArgumentParser(description="regime_adaptive entry-filter study")
    sub = parser.add_subparsers(dest="cmd", required=True)
    sub.add_parser("verify-inputs")
    sub.add_parser("lock")
    sub.add_parser("coverage")
    for name in ("select", "judge", "smoke"):
        cmd = sub.add_parser(name)
        cmd.add_argument("--cache", required=True)
        cmd.add_argument("--work", default="/tmp/ra2017")
        if name != "smoke":
            cmd.add_argument("--workers", type=int, default=min(6, os.cpu_count() or 1))
    sub.add_parser("render")
    sub.add_parser("check")
    args = parser.parse_args(argv)
    if args.cmd == "verify-inputs":
        verify_inputs(write=True)
    elif args.cmd == "lock":
        lock_cmd()
    elif args.cmd == "coverage":
        coverage_cmd()
    elif args.cmd == "select":
        select_cmd(args.cache, args.work, args.workers)
    elif args.cmd == "judge":
        judge_cmd(args.cache, args.work, args.workers)
    elif args.cmd == "render":
        render_cmd()
    elif args.cmd == "smoke":
        smoke_cmd(args.cache, args.work)
    elif args.cmd == "check":
        require_lock()
        render_cmd()
    return 0


if __name__ == "__main__":
    raise SystemExit(main())

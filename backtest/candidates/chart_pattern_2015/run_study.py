#!/usr/bin/env python3

import argparse
import bisect
import copy
import csv
import gzip
import hashlib
import io
import json
import math
import multiprocessing
import os
import random
import re
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
CANDIDATES = os.path.join(BACKTEST, "candidates")
SOURCE_DIRS = {
    "breakout_2014": os.path.join(CANDIDATES, "breakout_2014"),
    "tema_cross_2016": os.path.join(CANDIDATES, "tema_cross_2016"),
}
PROTOCOL_PATH = os.path.join(HERE, "protocol.json")
RESEARCH_CONFIG_PATH = os.path.join(HERE, "research_config.json")
ARMS_DIR = os.path.join(HERE, "arms")
RESOLUTION_PATH = os.path.join(ARMS_DIR, "resolution.json")
STUDY_MANIFEST_PATH = os.path.join(HERE, "study_manifest.json")
COVERAGE_PATH = os.path.join(HERE, "coverage_report.json")
VALIDATION_PATH = os.path.join(HERE, "validation.json")
LOCK_PATH = os.path.join(HERE, "protocol_lock.json")
SELECTION_PATH = os.path.join(HERE, "selection.json")
RESULTS_PATH = os.path.join(HERE, "results.json")
SAMPLES_PATH = os.path.join(HERE, "samples.json.gz")
RUN_LOG_PATH = os.path.join(HERE, "run_log.json")
VERDICT_PATH = os.path.join(HERE, "verdict.json")
REPORT_PATH = os.path.join(HERE, "REPORT.md")
STUDY_ROOT = HERE
CACHE_ENV = "GO_TRADER_OHLCV_CACHE_DB"
DEFAULT_DB = os.path.join(REPO, "shared_tools", "trading_bot.db")
HOUR_MS = 3_600_000
DAY_MS = 86_400_000
TF_MS = {"30m": 1_800_000, "1h": 3_600_000, "4h": 14_400_000}
STRATEGY = "chart_pattern"
SIGNAL_FILES = (
    "shared_strategies/open/chart_patterns.py",
    "shared_strategies/open/mtf_confluence.py",
    "shared_strategies/open/indicators_core.py",
    "shared_strategies/open/registry.py",
    "shared_strategies/open/spot/strategies.py",
    "shared_strategies/open/futures/strategies.py",
    "shared_tools/strategy_composition.py",
)
ENGINE_FILES = (
    "backtest/backtester.py",
    "backtest/eval_windows.py",
    "backtest/run_backtest.py",
    "backtest/tune_live.py",
    "backtest/offline_manifest.py",
    "backtest/regime_label_columns.py",
    "backtest/registry_loader.py",
    "shared_tools/regime.py",
    "shared_tools/atr.py",
    "shared_tools/funding_fetcher.py",
    "shared_tools/data_fetcher.py",
    "shared_tools/storage.py",
    "shared_tools/closed_bar.py",
    "shared_strategies/close/trailing_tp_ratchet.py",
    "shared_strategies/close/regime_atr.py",
    "shared_strategies/close/registry.py",
    "shared_scripts/check_hyperliquid.py",
    "scheduler/regime_multi_window.go",
    "scheduler/hl_batch.go",
    "uv.lock",
)
LABELLER_FILES = (
    "backtest/regime_label_columns.py",
    "shared_tools/regime.py",
    "shared_tools/atr.py",
    "shared_strategies/open/adx_trend.py",
    "shared_strategies/open/indicators_core.py",
)
DRIVER_FILES = (
    "backtest/candidates/chart_pattern_2015/run_study.py",
    "backtest/candidates/chart_pattern_2015/render_report.py",
)
for _p in (os.path.join(REPO, "shared_tools"), BACKTEST, os.path.join(REPO, "shared_strategies", "open")):
    if _p not in sys.path:
        sys.path.insert(0, _p)


class StudyError(SystemExit):
    pass


_ROOT_FILES = {
    "PROTOCOL_PATH": "protocol.json", "RESEARCH_CONFIG_PATH": "research_config.json", "ARMS_DIR": "arms",
    "STUDY_MANIFEST_PATH": "study_manifest.json", "COVERAGE_PATH": "coverage_report.json",
    "VALIDATION_PATH": "validation.json", "LOCK_PATH": "protocol_lock.json", "SELECTION_PATH": "selection.json",
    "RESULTS_PATH": "results.json", "SAMPLES_PATH": "samples.json.gz", "RUN_LOG_PATH": "run_log.json",
    "VERDICT_PATH": "verdict.json", "REPORT_PATH": "REPORT.md",
}


def set_study_root(root: str) -> None:
    g = globals()
    root = os.path.abspath(root)
    for name, leaf in _ROOT_FILES.items():
        g[name] = os.path.join(root, leaf)
    g["RESOLUTION_PATH"] = os.path.join(g["ARMS_DIR"], "resolution.json")
    g["STUDY_ROOT"] = root


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
    tmp = path + f".{os.getpid()}.tmp"
    with open(tmp, "w") as fh:
        fh.write(dumps(obj))
    os.replace(tmp, path)


def write_gz_json(path: str, obj) -> None:
    tmp = path + f".{os.getpid()}.tmp"
    with open(tmp, "wb") as fh:
        with gzip.GzipFile(filename="", mode="wb", fileobj=fh, mtime=0) as gz:
            gz.write(json.dumps(obj, sort_keys=True, separators=(",", ":"), default=str).encode())
    os.replace(tmp, path)


def read_gz_json(path: str):
    with gzip.open(path, "rt") as fh:
        return json.load(fh)


def read_gz_csv(path: str) -> list:
    with gzip.open(path, "rt") as fh:
        rows = list(csv.reader(fh))
    return rows[1:]


def rel(path: str) -> str:
    return os.path.relpath(os.path.abspath(path), REPO)


def repo_path(relpath: str) -> str:
    return os.path.join(REPO, relpath)


def git_head() -> str:
    try:
        head = subprocess.check_output(["git", "-C", REPO, "rev-parse", "HEAD"], text=True).strip()
        dirty = subprocess.check_output(["git", "-C", REPO, "status", "--porcelain", "--untracked-files=no"],
                                        text=True).strip()
    except (OSError, subprocess.CalledProcessError):
        return "unknown"
    return head + ("+uncommitted-changes" if dirty else "")


def git_last_commit(path: str) -> str:
    try:
        return subprocess.check_output(["git", "-C", REPO, "log", "-1", "--format=%H", "--", rel(path)],
                                       text=True).strip() or "untracked"
    except (OSError, subprocess.CalledProcessError):
        return "unknown"


def now_utc() -> str:
    return time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime())


def to_ms(day: str) -> int:
    import pandas as pd
    return int(pd.Timestamp(day, tz="UTC").timestamp() * 1000)


def ms_str(ms) -> str:
    import pandas as pd
    return str(pd.Timestamp(int(ms), unit="ms"))


def protocol() -> dict:
    return load_json(PROTOCOL_PATH)


def protocol_sha256() -> str:
    return sha256_file(PROTOCOL_PATH)


def files_digest(paths) -> dict:
    return {p: sha256_file(repo_path(p)) for p in paths}


def dependency_sha256() -> dict:
    return {"signal": files_digest(SIGNAL_FILES), "engine": files_digest(ENGINE_FILES),
            "labeller": files_digest(LABELLER_FILES)}


def signal_fingerprint() -> str:
    return sha256_bytes(json.dumps(files_digest(SIGNAL_FILES), sort_keys=True).encode())


def engine_fingerprint() -> str:
    return sha256_bytes(json.dumps(dependency_sha256(), sort_keys=True).encode())


def labeller_sha256() -> str:
    return sha256_bytes(json.dumps(files_digest(LABELLER_FILES), sort_keys=True).encode())


def checkpoint_fresh(path: str, fingerprint: str):
    if not os.path.exists(path):
        return None
    try:
        held = load_json(path)
    except (OSError, ValueError):
        return None
    return held if held.get("fingerprint") == fingerprint else None


def refuse_default_db(path: str) -> str:
    path = os.path.abspath(path)
    if os.path.realpath(path) == os.path.realpath(DEFAULT_DB):
        raise StudyError(f"refusing the repository trading database {DEFAULT_DB}")
    if os.path.exists(path):
        raise StudyError(f"refusing to reuse an existing cache {path}; pass a new path")
    return path


def set_cache(path: str) -> None:
    os.environ[CACHE_ENV] = os.path.abspath(path)


NETWORK_CALLS = []


def refuse_network(*_a, **_k):
    NETWORK_CALLS.append(1)
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
    data_fetcher.get_exchange = refuse_network
    funding_fetcher._hl_adapter = refuse_network
    import run_backtest
    run_backtest.get_exchange = refuse_network


def network_probe() -> dict:
    out = {}
    try:
        socket.create_connection(("api.hyperliquid.xyz", 443), timeout=5)
        out["socket_connect"] = "connected (unexpected)"
    except RuntimeError as exc:
        out["socket_connect"] = f"refused: {exc}"
    try:
        socket.getaddrinfo("api.binance.us", 443)
        out["name_resolution"] = "resolved (unexpected)"
    except RuntimeError as exc:
        out["name_resolution"] = f"refused: {exc}"
    from data_fetcher import load_cached_data
    try:
        load_cached_data("DOGE/USDT", "1h", start_date="2024-01-01", end_date="2024-02-01")
        out["unfrozen_symbol_load"] = "loaded (unexpected)"
    except RuntimeError as exc:
        out["unfrozen_symbol_load"] = f"refused: {exc}"
    if any("unexpected" in v for v in out.values()):
        raise StudyError(f"network probe failed: {out}")
    return out


def windows(proto: dict) -> dict:
    return {k: tuple(v) for k, v in proto["windows"].items()}


def proxy_symbol(coin: str) -> str:
    return f"{coin}/USDT"


def proxy_rel(coin: str, tf: str) -> str:
    return f"proxy/{coin}_{tf}_candles.csv.gz"


def funding_rel(coin: str) -> str:
    return f"proxy/{coin}_funding.csv.gz"


def input_plan(proto: dict) -> dict:
    out = {}
    for coin in proto["perps"]["coins"]:
        for tf in proto["perps"]["intervals"]:
            out[proxy_rel(coin, tf)] = "breakout_2014"
        out[funding_rel(coin)] = "breakout_2014"
    for coin, tf in proto["audit"]["datasets"]:
        name = proxy_rel(coin, tf)
        if name not in out:
            out[name] = "tema_cross_2016" if tf == "4h" else "breakout_2014"
    return dict(sorted(out.items()))


def manifest_paths(proto: dict) -> dict:
    out = {}
    for g, spec in sorted(proto["perps"]["groups"].items()):
        if spec.get("manifest"):
            out[g] = os.path.join(SOURCE_DIRS["breakout_2014"], spec["manifest"])
    return out


def freeze_inputs() -> None:
    import offline_manifest as om
    proto = protocol()
    if os.path.exists(STUDY_MANIFEST_PATH):
        raise StudyError("study_manifest.json exists; the input freeze runs once")
    src_manifests = {s: load_json(os.path.join(d, "study_manifest.json")) for s, d in SOURCE_DIRS.items()}
    files = {}
    for name, study in input_plan(proto).items():
        meta = src_manifests[study]["files"].get(name)
        if meta is None:
            raise StudyError(f"{study} study_manifest.json lists no {name}")
        path = os.path.join(SOURCE_DIRS[study], "data", name)
        got = sha256_file(path)
        if got != meta["sha256"]:
            raise StudyError(f"{rel(path)} sha256 {got} != {study} manifest {meta['sha256']}")
        files[name] = {**meta, "path": rel(path), "source_study": study}
    manifests = {}
    for group, path in manifest_paths(proto).items():
        m = om.load_manifest(path, verify=True)
        manifests[group] = {
            "path": rel(path), "sha256": sha256_file(path), "source_study": "breakout_2014",
            "source_revision": git_last_commit(path), "interval": m["interval"],
            "warmup_bars": m["warmup_bars"], "windows": {k: [v["start"], v["end"]] for k, v in m["windows"].items()},
            "datasets": {d["coin"]: {"candles": d["candles"], "funding": d["funding"]} for d in m["datasets"]},
        }
    write_json(STUDY_MANIFEST_PATH, {
        "study": proto["study"], "pinned_end": proto["inputs"]["pinned_end"], "frozen_at_utc": now_utc(),
        "source_commit": git_head(), "acquisition": "none: every input is read in place from its source study",
        "sources": {s: {"study_manifest": rel(os.path.join(d, "study_manifest.json")),
                        "study_manifest_sha256": sha256_file(os.path.join(d, "study_manifest.json")),
                        "source_revision": git_last_commit(os.path.join(d, "study_manifest.json"))}
                    for s, d in sorted(SOURCE_DIRS.items())},
        "files": files, "manifests": manifests,
    })
    print(f"froze {len(files)} proxy files and {len(manifests)} offline manifests in place")


def verify_inputs(manifest_path: str = None) -> dict:
    import offline_manifest as om
    manifest_path = manifest_path or STUDY_MANIFEST_PATH
    if not os.path.exists(manifest_path):
        raise StudyError("study_manifest.json is missing; run freeze-inputs")
    sm = load_json(manifest_path)
    for s, meta in sorted(sm["sources"].items()):
        got = sha256_file(repo_path(meta["study_manifest"]))
        if got != meta["study_manifest_sha256"]:
            raise StudyError(f"source manifest {meta['study_manifest']} sha256 {got} != frozen "
                             f"{meta['study_manifest_sha256']}")
    for name, meta in sorted(sm["files"].items()):
        got = sha256_file(repo_path(meta["path"]))
        if got != meta["sha256"]:
            raise StudyError(f"input {meta['path']} sha256 {got} != frozen {meta['sha256']}")
    loaded = {}
    for group, meta in sorted(sm["manifests"].items()):
        path = repo_path(meta["path"])
        got = sha256_file(path)
        if got != meta["sha256"]:
            raise StudyError(f"manifest {meta['path']} sha256 {got} != frozen {meta['sha256']}")
        loaded[group] = om.load_manifest(path, verify=True)
    return {"study_manifest": sm, "manifests": loaded}


def build_cache(cache_db: str, sm: dict) -> None:
    set_cache(cache_db)
    import pandas as pd
    import storage
    storage.init_db(cache_db)
    for name, meta in sorted(sm["files"].items()):
        rows = read_gz_csv(repo_path(meta["path"]))
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


def candle_frame(path: str):
    import pandas as pd
    rows = read_gz_csv(path)
    df = pd.DataFrame(rows, columns=["timestamp", "open", "high", "low", "close", "volume"])
    df = df.astype({"timestamp": "int64", "open": float, "high": float, "low": float, "close": float,
                    "volume": float})
    df.index = pd.to_datetime(df["timestamp"].to_numpy(), unit="ms")
    df.index.name = "datetime"
    return df


def go_frame_length(regime_period: int = 14) -> int:
    src = open(repo_path("scheduler/regime_multi_window.go")).read()
    base = int(re.search(r"regimeOhlcvBaseLimit\s*=\s*(\d+)", src).group(1))
    margin = int(re.search(r"regimeOhlcvMargin\s*=\s*(\d+)", src).group(1))
    batch = open(repo_path("scheduler/hl_batch.go")).read()
    batch_default = int(re.search(r"hlBatchPythonDefaultOhlcvLimit\s*=\s*(\d+)", batch).group(1))
    if "limit = regimeRequiredOhlcvLimit(rc)" not in batch:
        raise StudyError("scheduler/hl_batch.go no longer derives the frame from regimeRequiredOhlcvLimit")
    if batch_default != base:
        raise StudyError(f"hl_batch default limit {batch_default} != regimeOhlcvBaseLimit {base}")
    period = max(14, int(regime_period))
    return max(base, 2 * period + 1 + margin)


FINITE_FACTORS = (2, 3, 4, 5, 6)


def gate_signal(raw: int, trend: int, mode: str = "veto") -> int:
    if mode == "align":
        if (raw == 1 and trend != 1) or (raw == -1 and trend != -1):
            return 0
        return raw
    if (raw == 1 and trend == -1) or (raw == -1 and trend == 1):
        return 0
    return raw


def decide(frame, params: dict, factors=FINITE_FACTORS) -> dict:
    from chart_patterns import _htf_gate_trend, chart_pattern_core
    base = {k: v for k, v in params.items() if not k.startswith("htf_gate_")}
    res = chart_pattern_core(frame, htf_gate_factor=0, **base)
    raw = int(res["signal"].iat[-1])
    out = {"raw": raw}
    for f in factors:
        trend = _htf_gate_trend(res, int(f), params["htf_gate_ema_fast"], params["htf_gate_ema_slow"])
        out[f"t{f}"] = int(trend.iat[-1])
    return out


def table_columns() -> list:
    return ["ts", "close", "raw"] + [f"t{f}" for f in FINITE_FACTORS]


def table_fingerprint(source_sha: str, length: int, params: dict, start_ms: int) -> str:
    return sha256_bytes(json.dumps({"source": source_sha, "length": length, "params": params,
                                    "start_ms": start_ms, "signal": signal_fingerprint(),
                                    "factors": list(FINITE_FACTORS)}, sort_keys=True).encode())


def table_path(work_dir: str, source_sha: str) -> str:
    return os.path.join(work_dir, "tables", f"{source_sha}.json.gz")


def _table_chunk(task):
    path, length, params, lo, hi = task
    df = _TABLE_SOURCES.get(path)
    if df is None:
        df = candle_frame(path)
        _TABLE_SOURCES[path] = df
    rows = []
    ts = df["timestamp"].to_numpy()
    close = df["close"].to_numpy()
    for p in range(lo, hi):
        d = decide(df.iloc[p - length + 1:p + 1], params)
        rows.append([int(ts[p]), float(close[p]), d["raw"]] + [d[f"t{f}"] for f in FINITE_FACTORS])
    return path, lo, rows


_TABLE_SOURCES = {}


def table_sources(proto: dict, sm: dict) -> dict:
    out = {}
    starts = [to_ms(proto["windows"][w][0]) for w in proto["perps"]["coverage_windows"] if w != "2023"]
    first = min(starts)
    for coin in proto["perps"]["coins"]:
        for tf in proto["perps"]["intervals"]:
            meta = sm["files"][proxy_rel(coin, tf)]
            out[meta["sha256"]] = {"path": meta["path"], "start_ms": first - (proto["frame"]["warmup_bars"] + 2)
                                   * TF_MS[tf], "kind": "proxy", "dataset": f"{coin} {tf}"}
    for group, m in sorted(sm["manifests"].items()):
        base = os.path.dirname(repo_path(m["path"]))
        for coin, ds in sorted(m["datasets"].items()):
            path = os.path.normpath(os.path.join(base, ds["candles"]["path"]))
            sha = ds["candles"]["sha256"]
            if sha in out:
                continue
            out[sha] = {"path": rel(path), "start_ms": 0, "kind": group.split("_")[0], "dataset": f"{coin} "
                        f"{m['interval']}"}
    return dict(sorted(out.items()))


def build_tables(work_dir: str, workers: int, only: set = None) -> dict:
    proto = protocol()
    sm = verify_inputs()["study_manifest"]
    block_network()
    length = proto["frame"]["length_bars"]
    params = proto["signal_params"]
    os.makedirs(os.path.join(work_dir, "tables"), exist_ok=True)
    tasks, plan = [], {}
    for sha, src in table_sources(proto, sm).items():
        if only and sha not in only:
            continue
        path = repo_path(src["path"])
        fp = table_fingerprint(sha, length, params, src["start_ms"])
        tp = table_path(work_dir, sha)
        if os.path.exists(tp) and read_gz_json(tp).get("fingerprint") == fp:
            continue
        df = candle_frame(path)
        ts = df["timestamp"].tolist()
        lo = max(length - 1, bisect.bisect_left(ts, src["start_ms"]))
        hi = len(ts)
        plan[sha] = {"path": path, "fp": fp, "rows": {}, "src": src, "lo": lo, "hi": hi}
        step = 1500
        for a in range(lo, hi, step):
            tasks.append((path, length, params, a, min(hi, a + step)))
    print(f"[tables] {len(plan)} sources, {len(tasks)} chunks, {workers} workers", flush=True)
    started = time.time()
    if tasks:
        ctx = multiprocessing.get_context("spawn")
        with ctx.Pool(processes=max(1, min(workers, len(tasks))), initializer=_table_init) as pool:
            for i, (path, lo, rows) in enumerate(pool.imap_unordered(_table_chunk, tasks), 1):
                for sha, pl in plan.items():
                    if pl["path"] == path:
                        pl["rows"][lo] = rows
                if i % 20 == 0 or i == len(tasks):
                    print(f"[tables] {i}/{len(tasks)} chunks (elapsed {round(time.time() - started)}s)", flush=True)
    for sha, pl in plan.items():
        rows = [r for lo in sorted(pl["rows"]) for r in pl["rows"][lo]]
        if len(rows) != pl["hi"] - pl["lo"]:
            raise StudyError(f"table {sha}: {len(rows)} rows != {pl['hi'] - pl['lo']}")
        write_gz_json(table_path(work_dir, sha), {
            "fingerprint": pl["fp"], "source": rel(pl["path"]), "source_sha256": sha, "length": length,
            "params": params, "columns": table_columns(), "rows": rows, "built_utc": now_utc(),
            "seconds": round(time.time() - started, 1)})
    return {sha: table_path(work_dir, sha) for sha in table_sources(proto, sm)}


def _table_init() -> None:
    for p in (os.path.join(REPO, "shared_tools"), BACKTEST, os.path.join(REPO, "shared_strategies", "open")):
        if p not in sys.path:
            sys.path.insert(0, p)
    block_network()


class SignalTable:
    def __init__(self, doc: dict):
        cols = doc["columns"]
        self.length = doc["length"]
        self.params = doc["params"]
        self.source = doc["source"]
        self.by_ts = {}
        for r in doc["rows"]:
            rec = dict(zip(cols, r))
            self.by_ts[int(rec["ts"])] = rec
        self.first_ts = min(self.by_ts) if self.by_ts else None

    def signal(self, ts: int, factor: int, mode: str):
        rec = self.by_ts.get(int(ts))
        if rec is None:
            return None
        if factor and int(factor) > 1:
            trend = rec.get(f"t{int(factor)}")
            if trend is None:
                raise StudyError(f"table {self.source} has no trend for factor {factor}")
            return gate_signal(int(rec["raw"]), int(trend), mode), int(trend), rec["close"]
        return int(rec["raw"]), None, rec["close"]


class FiniteFrameRegistry:
    def __init__(self, base, table: SignalTable, required_from_ms: int):
        self._base = base
        self.table = table
        self.required_from_ms = int(required_from_ms)
        self.STRATEGY_REGISTRY = base.STRATEGY_REGISTRY
        self.calls = 0

    def __getattr__(self, name):
        return getattr(self._base, name)

    def apply_strategy(self, name, df, params=None):
        import numpy as np
        if name != STRATEGY:
            raise StudyError(f"finite-frame registry serves {STRATEGY} only, got {name}")
        p = {**self._base.STRATEGY_REGISTRY[name]["default_params"], **(params or {})}
        for key, value in self.table.params.items():
            if p.get(key) != value:
                raise StudyError(f"finite-frame table was built with {key}={value}, leg asks {p.get(key)}")
        factor = int(p.get("htf_gate_factor") or 0)
        mode = p["htf_gate_mode"]
        out = df.copy()
        ts = (df.index.values.astype("datetime64[ms]").astype("int64"))
        sig = np.zeros(len(df), dtype=int)
        trend = np.zeros(len(df), dtype=int)
        close = df["close"].to_numpy(dtype=float)
        missing = []
        for i, t in enumerate(ts):
            got = self.table.signal(int(t), factor, mode)
            if got is None:
                if int(t) >= self.required_from_ms:
                    missing.append(int(t))
                continue
            s, tr, c = got
            if c != close[i]:
                raise StudyError(f"leg candle at {ms_str(t)} close {close[i]} differs from the table source "
                                 f"{self.table.source} close {c}")
            sig[i] = s
            trend[i] = tr or 0
        if missing:
            raise StudyError(f"finite-frame table {self.table.source} lacks {len(missing)} decision bars from "
                             f"{ms_str(missing[0])}")
        self.calls += 1
        out["signal"] = sig
        if factor > 1:
            out["htf_gate_trend"] = trend
        out["swing_high"] = np.nan
        out["swing_low"] = np.nan
        return out


def load_table(work_dir: str, sha: str) -> SignalTable:
    path = table_path(work_dir, sha)
    if not os.path.exists(path):
        raise StudyError(f"signal table {path} is missing; run `run_study.py tables`")
    proto = protocol()
    doc = read_gz_json(path)
    tables = table_sources(proto, load_json(STUDY_MANIFEST_PATH))
    src = tables.get(sha)
    if src is None:
        raise StudyError(f"signal table {sha} is not a study source")
    want = table_fingerprint(sha, proto["frame"]["length_bars"], proto["signal_params"], src["start_ms"])
    if doc.get("fingerprint") != want:
        raise StudyError(f"signal table {path} is stale (fingerprint differs); rebuild it with `tables`")
    return SignalTable(doc)


def research_config() -> dict:
    return load_json(RESEARCH_CONFIG_PATH)


def contract_arm_path(contract: str, factor: int) -> str:
    return os.path.join(ARMS_DIR, contract, f"f{factor}.json")


def arm_factors(proto: dict) -> list:
    return sorted(set([0] + list(proto["perps"]["factors_screened"])))


def build_arm_doc(proto: dict, contract: str, factor: int) -> dict:
    if contract == "audit":
        return {"name": STRATEGY, "direction": "long", "params": {"htf_gate_factor": factor}}
    spec = proto["perps"]["contracts"][contract]
    if spec["kind"] == "open_as_close":
        return {"name": STRATEGY, "type": "perps", "direction": spec["direction"],
                "params": {"htf_gate_factor": factor}}
    cfg = copy.deepcopy(research_config())
    for sc in cfg["strategies"]:
        sc["direction"] = spec["direction"]
        sc["open_strategy"] = {"name": STRATEGY, "params": {"htf_gate_factor": factor}}
    return cfg


def config_strategy_id(cfg: dict, coin: str, tf: str) -> str:
    hits = [sc["id"] for sc in cfg["strategies"] if sc["args"][1] == coin and sc["args"][2] == tf]
    if len(hits) != 1:
        raise StudyError(f"expected one strategy for {coin} {tf}, found {hits}")
    return hits[0]


def resolve_config_arm(path: str, coin: str, tf: str) -> tuple:
    import eval_windows as ew
    from run_backtest import load_strategy_config
    from tune_live import build_candidate
    cfg = load_json(path)
    sid = config_strategy_id(cfg, coin, tf)
    sc = next(s for s in cfg["strategies"] if s["id"] == sid)
    res = load_strategy_config(path, sid, inject_user_defaults=True)
    open_ref = res["open_strategy"]
    cand = build_candidate(open_ref["name"], open_ref.get("params") or {}, res)
    if sc.get("htf_filter"):
        cand["htf_filter"] = True
    cand["atr_method"] = res["atr_method"]
    consumers = [k for k in ("allowed_regimes", "regime_directional_policy", "regime_side") if cand.get(k)]
    if not consumers and not cand.get("regime_windows_spec"):
        for key, default in (("regime_period", 14), ("regime_adx_threshold", 20.0)):
            if key in cand:
                if cand[key] != default:
                    raise StudyError(f"{path} {sid}: {key}={cand[key]} differs from the harness default {default}")
                cand.pop(key)
    ew.validate_candidate(cand)
    return cand, res, sc


def arm_candidate(proto: dict, contract: str, factor: int, coin: str, tf: str) -> dict:
    import eval_windows as ew
    path = contract_arm_path(contract, factor)
    if contract == "audit" or proto["perps"]["contracts"][contract]["kind"] == "open_as_close":
        cand = load_json(path)
        ew.validate_candidate(copy.deepcopy(cand))
        return cand
    return resolve_config_arm(path, coin, tf)[0]


def contracts(proto: dict) -> list:
    return sorted(proto["perps"]["contracts"])


def all_arm_files(proto: dict) -> dict:
    want = {}
    for f in proto["audit"]["factors"]:
        want[contract_arm_path("audit", f)] = build_arm_doc(proto, "audit", f)
    for c in contracts(proto):
        for f in arm_factors(proto):
            want[contract_arm_path(c, f)] = build_arm_doc(proto, c, f)
    return want


def strip_factor(cand: dict) -> dict:
    out = copy.deepcopy(cand)
    params = dict(out.get("params") or {})
    params.pop("htf_gate_factor", None)
    out["params"] = params
    return out


def resolved_ladders(labels: list) -> dict:
    close_dir = repo_path("shared_strategies/close")
    if close_dir not in sys.path:
        sys.path.insert(0, close_dir)
    from trailing_tp_ratchet import resolve_tiers_for_regime
    from regime_atr import REGIME_ATR_DEFAULTS_TRAILING
    out = {}
    for label in labels:
        tiers, errs = resolve_tiers_for_regime({"use_defaults": True}, label, regime_table=True)
        trail = REGIME_ATR_DEFAULTS_TRAILING.get(label)
        out[label] = {"tiers": [{"atr_multiple": m, "close_fraction": f, "trailing_mult_after": t}
                                for m, f, t in tiers], "errors": errs,
                      "trailing_stop_atr_multiple": trail.atr if trail else None}
    return out


def build_arms(check: bool) -> dict:
    import eval_windows as ew
    proto = protocol()
    length = go_frame_length(14)
    if length != proto["frame"]["length_bars"]:
        raise StudyError(f"the Go scheduler derives a {length}-bar frame; protocol says "
                         f"{proto['frame']['length_bars']}")
    want = {p: dumps(doc) for p, doc in all_arm_files(proto).items()}
    bad = []
    for path, text in sorted(want.items()):
        os.makedirs(os.path.dirname(path), exist_ok=True)
        if check:
            if not os.path.exists(path) or open(path).read() != text:
                bad.append(rel(path))
        else:
            with open(path, "w") as fh:
                fh.write(text)
    if bad:
        raise StudyError(f"arm files differ from the pure build of protocol.json and research_config.json: {bad}")
    report = {"frame_length_bars": length, "contracts": {}, "refused": {}}
    for name, why in sorted(proto["perps"]["refused_contracts"].items()):
        try:
            ew.validate_candidate({"name": STRATEGY, "type": "perps", "direction": "both",
                                   "params": {"htf_gate_factor": 4}})
            raise StudyError(f"{name}: the harness accepted a bidirectional open-as-close candidate")
        except ValueError as exc:
            report["refused"][name] = {"refused_by": "eval_windows.validate_candidate", "error": str(exc)[:240],
                                       "protocol_reason": why}
    datasets = [(c, tf) for c in proto["perps"]["coins"] for tf in proto["perps"]["intervals"]]
    for contract in contracts(proto):
        spec = proto["perps"]["contracts"][contract]
        rows = {}
        for coin, tf in datasets:
            base = None
            for f in arm_factors(proto):
                cand = arm_candidate(proto, contract, f, coin, tf)
                if int((cand.get("params") or {}).get("htf_gate_factor", -1)) != f:
                    raise StudyError(f"{contract} f{f} {coin} {tf}: candidate factor {cand.get('params')}")
                if base is None:
                    base = strip_factor(cand)
                elif strip_factor(cand) != base:
                    raise StudyError(f"{contract} {coin} {tf}: factor {f} differs from factor 0 beyond the gate")
            cand = arm_candidate(proto, contract, 4, coin, tf)
            if cand["direction"] != spec["direction"]:
                raise StudyError(f"{contract}: direction {cand['direction']} != {spec['direction']}")
            row = {"direction": cand["direction"], "type": cand.get("type"),
                   "close": cand.get("close_strategies"), "params_except_factor": base.get("params"),
                   "atr_method": cand.get("atr_method") or "simple", "htf_filter": bool(cand.get("htf_filter")),
                   "allowed_regimes": cand.get("allowed_regimes"), "stop_loss_atr_mult": cand.get("stop_loss_atr_mult"),
                   "trailing_stop_atr_mult": cand.get("trailing_stop_atr_mult")}
            if spec["kind"] == "ratchet":
                c2, res, sc = resolve_config_arm(contract_arm_path(contract, 4), coin, tf)
                plan = c2.get("regime_label_windows") or {}
                if int(plan.get("limit") or 0) != length:
                    raise StudyError(f"{contract} {coin} {tf}: label limit {plan.get('limit')} != frame {length}")
                checks = {
                    "direction": (res["direction"], c2["direction"]),
                    "close_strategies": (res["close_strategies"], c2["close_strategies"]),
                    "trailing_stop_atr_mult_regime": (res["trailing_stop_atr_mult_regime"],
                                                      c2["stop_context"].get("trailing_stop_atr_mult_regime")),
                    "regime_enabled": (res["regime_enabled"], c2.get("regime_enabled")),
                    "regime_period": (res["regime_period"], int(c2.get("regime_period") or 14)),
                    "atr_method": (res["atr_method"], c2.get("atr_method")),
                    "leverage": (res["leverage"], c2["stop_context"].get("leverage")),
                }
                diff = {k: v for k, v in checks.items() if v[0] != v[1]}
                if diff:
                    raise StudyError(f"{contract} {coin} {tf}: resolved settings differ from engine arguments {diff}")
                vocab = sorted((plan.get("vocabularies") or {}).get("atr") or [])
                row.update({
                    "trailing_stop_atr_mult_regime": res["trailing_stop_atr_mult_regime"],
                    "resolved_stop_owner": "trailing_stop_atr_mult_regime (use_defaults)",
                    "regime": {"enabled": res["regime_enabled"], "classifier": plan.get("classifiers"),
                               "period": res["regime_period"], "adx_threshold": res["regime_adx_threshold"],
                               "windows": plan.get("windows"), "labels": vocab,
                               "label_limit": plan.get("limit"), "gate_on_failure": res["regime_gate_on_failure"]},
                    "resolved_per_label": resolved_ladders(vocab),
                    "perps_sizing": res.get("perps_sizing"), "leverage": res.get("leverage"),
                    "max_drawdown_pct_engine": res.get("max_drawdown_pct"),
                    "regime_directional_policy": res.get("regime_directional_policy"),
                    "invert_signal": res.get("invert_signal"),
                    "carried_by_study": {
                        "atr_method": "copied from the resolution (build_candidate does not copy it)",
                        "htf_filter": "false in the configuration; copied when true",
                        "regime_period": "build_candidate writes regime_period 14 and regime_adx_threshold 20; "
                                         "validate_candidate refuses them without a gate consumer, so the study "
                                         "drops them; run_candidate_leg then uses the same defaults (14, 20)",
                        "closed_bar_decisions": f"{sc.get('closed_bar_decisions')} in the configuration; the "
                                                "candidate has no field for it, so the study's finite-frame "
                                                "signal path models it (closed-bar frame of the last "
                                                f"{length} candles, next-bar fill)"},
                    "unsupported_or_unmodeled": ["shared-wallet pool sizing", "forming-candle decisions",
                                                 "venue lot and minimum-notional gates on the proxy path"],
                })
            rows[f"{coin} {tf}"] = row
        first = next(iter(rows.values()))
        report["contracts"][contract] = {"kind": spec["kind"], "direction": spec["direction"],
                                         "per_dataset_identical": all(
                                             {k: v for k, v in r.items()} == first for r in rows.values()),
                                         "resolved": first}
    for coin, tf in proto["audit"]["datasets"]:
        for f in proto["audit"]["factors"]:
            cand = arm_candidate(proto, "audit", f, coin, tf)
            if strip_factor(cand) != strip_factor(arm_candidate(proto, "audit", 0, coin, tf)):
                raise StudyError("audit arms differ beyond the gate factor")
    report["audit"] = {"candidate": strip_factor(arm_candidate(proto, "audit", 0, "BTC", "1h")),
                       "factors": proto["audit"]["factors"], "registry": proto["audit"]["registry"]}
    text = dumps(report)
    if check:
        if not os.path.exists(RESOLUTION_PATH) or open(RESOLUTION_PATH).read() != text:
            raise StudyError("arms/resolution.json differs from the resolution of the arm files")
    else:
        with open(RESOLUTION_PATH, "w") as fh:
            fh.write(text)
    for c, r in sorted(report["contracts"].items()):
        res = r["resolved"]
        print(f"{c}: {r['kind']} dir {res['direction']} close {[x['name'] for x in res['close'] or []]} "
              f"atr {res['atr_method']} identical across datasets {r['per_dataset_identical']}")
    for c, r in sorted(report["refused"].items()):
        print(f"{c}: refused ({r['error'][:80]}...)")
    print(f"frame {length} bars; " + ("arms ok" if check else "arms written"))
    return report


def arms_sha256() -> dict:
    out = {}
    for root, _dirs, names in os.walk(ARMS_DIR):
        for name in sorted(names):
            if name.endswith(".json"):
                p = os.path.join(root, name)
                out[os.path.relpath(p, ARMS_DIR)] = sha256_file(p)
    return dict(sorted(out.items()))


def prefix_ok(ts: list, start_ms: int, tf: str, warmup: int) -> dict:
    step = TF_MS[tf]
    i = bisect.bisect_left(ts, start_ms)
    want = [start_ms - step * k for k in range(warmup, 0, -1)]
    got = ts[max(0, i - warmup):i]
    missing = sorted(set(want) - set(got))
    return {"ok": got == want, "rows_before_start": i, "missing": len(missing),
            "first_missing": ms_str(missing[0]) if missing else None}


def proxy_window_status(sm: dict, coin: str, tf: str, start: str, end: str, warm: int, funding: bool) -> dict:
    files = sm["files"]
    ts = [int(r[0]) for r in read_gz_csv(repo_path(files[proxy_rel(coin, tf)]["path"]))]
    tsset = set(ts)
    s, e = to_ms(start), to_ms(end)
    expected = list(range(s, e, TF_MS[tf]))
    present = sum(1 for t in expected if t in tsset)
    pre = prefix_ok(ts, s, tf, warm) if warm else {"ok": True, "missing": 0, "first_missing": None}
    item = {"start": start, "end": end, "expected_bars": len(expected), "present_bars": present,
            "missing_bars": len(expected) - present, "warmup": pre}
    why = []
    if not pre["ok"]:
        why.append(f"warm-up lacks {warm} consecutive bars ({pre['missing']} missing, first {pre['first_missing']})")
    if present == 0:
        why.append("no candles in the window")
    if funding:
        fmeta = files[funding_rel(coin)]
        priced = {int(t) // HOUR_MS * HOUR_MS for t, _ in read_gz_csv(repo_path(fmeta["path"]))}
        absent = {h for h, _ in fmeta["venue_absent_hours"]}
        unpriced = [h for h in range(s, e, HOUR_MS) if h not in priced and h not in absent]
        item.update({"unpriced_funding_hours": len(unpriced),
                     "first_unpriced_hour": ms_str(unpriced[0]) if unpriced else None,
                     "venue_absent_hours": sum(1 for h in range(s, e, HOUR_MS) if h in absent),
                     "first_venue_rate": fmeta.get("first_venue_rate")})
        if unpriced:
            why.append(f"{len(unpriced)} unpriced funding hours (first {item['first_unpriced_hour']}; "
                       f"venue funding starts {fmeta.get('first_venue_rate')})")
    item["status"] = "excluded" if why else "included"
    item["reason"] = "; ".join(why) if why else "complete warm-up prefix, candles present" + (
        ", every scored hour priced or proven venue-absent" if funding else "")
    return item


def coverage() -> dict:
    import offline_manifest as om
    proto = protocol()
    frozen = verify_inputs()
    sm = frozen["study_manifest"]
    block_network()
    wins = windows(proto)
    length = proto["frame"]["length_bars"]
    warm = proto["frame"]["warmup_bars"]
    rows = []
    for g, spec in sorted(proto["perps"]["groups"].items()):
        tf = spec["interval"]
        for coin in spec["members"]:
            row = {"section": "perps", "group": g, "kind": spec["kind"], "coin": coin, "interval": tf,
                   "windows": {}}
            if spec["kind"] == "proxy":
                for w in proto["perps"]["coverage_windows"]:
                    s, e = wins[w]
                    row["windows"][w] = proxy_window_status(sm, coin, tf, s, e, warm, True)
            else:
                m = frozen["manifests"][g]
                ds = next((d for d in m["datasets"] if d["coin"] == coin), None)
                w = spec["window"]
                if ds is None:
                    row["windows"][w] = {"status": "excluded", "reason": "no dataset in the manifest"}
                else:
                    win = m["windows"][w]
                    try:
                        ext, _, cov = om.window_frame(m, ds, w)
                        fcov = om.funding_coverage(om.load_funding(ds), win["start_ms"], win["end_ms"])
                        why = []
                        if not cov["complete"]:
                            why.append(f"{cov['missing_bars']} missing candles")
                        if cov["warmup_missing_bars"]:
                            why.append(f"{cov['warmup_missing_bars']} missing warm-up bars")
                        if m["warmup_bars"] < length - 1:
                            why.append(f"manifest warm-up {m['warmup_bars']} < {length - 1} frame bars")
                        if not fcov.get("complete"):
                            why.append(f"funding incomplete ({fcov.get('missing_hours')} missing hours)")
                        if spec["kind"] == "bridge" and not cov["complete"] and len(why) == 1:
                            why = []
                            note = (f"{cov['missing_bars']} missing proxy candles in the window are scored as they "
                                    "are (bridge rule)")
                        else:
                            note = None
                        row["windows"][w] = {"start": win["start"], "end": win["end"], "candles": cov,
                                             "funding": fcov, "status": "excluded" if why else "included",
                                             "reason": "; ".join(why) if why else (
                                                 note or "complete candles, warm-up and funding")}
                    except om.ManifestError as exc:
                        row["windows"][w] = {"status": "excluded", "reason": str(exc)}
            rows.append(row)
    for coin, tf in proto["audit"]["datasets"]:
        row = {"section": "audit", "group": "audit", "kind": "proxy", "coin": coin, "interval": tf, "windows": {}}
        all_w = sorted({w for memo in proto["audit"]["memos"].values() for w in memo["windows"]})
        for w in all_w:
            s, e = wins[w]
            item = proxy_window_status(sm, coin, tf, s, e, 0, True)
            item["note"] = ("the audit path loads the window with no warm-up; unpriced funding hours make the "
                            "charged-funding leg funding_incomplete (eval_windows verdict funding_incomplete)")
            item["status"] = "included" if item["present_bars"] else "excluded"
            row["windows"][w] = item
        rows.append(row)
    out = {"study": proto["study"], "protocol_sha256": protocol_sha256(),
           "study_manifest_sha256": sha256_file(STUDY_MANIFEST_PATH), "frame_length_bars": length,
           "warmup_bars": warm, "network_blocked": True, "network_calls": len(NETWORK_CALLS), "rows": rows}
    write_json(COVERAGE_PATH, out)
    for r in rows:
        stat = ", ".join(f"{w} {v['status'][:3]}" for w, v in sorted(r["windows"].items()))
        print(f"{r['section']:<6} {r['group']:<11} {r['coin']:<5} {r['interval']:<4} {stat}")
    return out


def included(cov: dict, group: str, coin: str, tf: str, window: str) -> bool:
    for r in cov["rows"]:
        if r["group"] == group and r["coin"] == coin and r["interval"] == tf:
            return r["windows"].get(window, {}).get("status") == "included"
    return False


class DiskLabelCache(MutableMapping):
    def __init__(self, root: str):
        self.root = root
        self.memo = {}
        os.makedirs(root, exist_ok=True)

    def _path(self, key: str) -> str:
        return os.path.join(self.root, key[:2], key + ".json.gz")

    def __contains__(self, key) -> bool:
        return key in self.memo or os.path.exists(self._path(key))

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


_EQUITY = {}


def install_equity_capture() -> None:
    import backtester
    if getattr(backtester.Backtester, "_study_2015_capture", False):
        return
    orig = backtester.Backtester._calculate_metrics

    def wrapped(self, equity_df, trades, df, timeframe):
        _EQUITY["last"] = equity_df["equity"].copy()
        return orig(self, equity_df, trades, df, timeframe)

    backtester.Backtester._calculate_metrics = wrapped
    backtester.Backtester._study_2015_capture = True


def daily_equity(series) -> list:
    if series is None or len(series) == 0:
        return []
    daily = series.groupby(series.index.normalize()).last()
    return [[str(d.date()), round(float(v), 6)] for d, v in daily.items()]


_WORKER = {}


def worker_init(base_db: str, work_dir: str) -> None:
    path = os.path.join(work_dir, "db", f"worker_{os.getpid()}.db")
    os.makedirs(os.path.dirname(path), exist_ok=True)
    shutil.copyfile(base_db, path)
    set_cache(path)
    for p in (os.path.join(REPO, "shared_tools"), BACKTEST, os.path.join(REPO, "shared_strategies", "open")):
        if p not in sys.path:
            sys.path.insert(0, p)
    block_network()
    install_equity_capture()
    from registry_loader import load_registry
    _WORKER["proto"] = protocol()
    _WORKER["regs"] = {"spot": load_registry("spot"), "futures": load_registry("futures")}
    _WORKER["labels"] = DiskLabelCache(os.path.join(work_dir, "labels", labeller_sha256()))
    _WORKER["manifests"] = verify_inputs()["manifests"]
    _WORKER["work_dir"] = work_dir
    _WORKER["tables"] = {}


def worker_table(sha: str) -> SignalTable:
    tables = _WORKER["tables"]
    if sha not in tables:
        tables.clear()
        tables[sha] = load_table(_WORKER["work_dir"], sha)
    return tables[sha]


def unit_source_sha(proto: dict, unit: dict) -> str:
    sm = load_json(STUDY_MANIFEST_PATH)
    if unit["kind"] == "proxy":
        return sm["files"][proxy_rel(unit["coin"], unit["interval"])]["sha256"]
    return sm["manifests"][unit["group"]]["datasets"][unit["coin"]]["candles"]["sha256"]


def compact_leg(leg: dict) -> dict:
    if leg is None:
        return None
    ex = leg.get("execution") or {}
    plist = ex.get("position_list") or []
    out = {k: leg.get(k) for k in ("return_pct", "sharpe", "max_dd_pct", "ddadj", "trades", "liquidated",
                                   "bh_return_pct", "span_days", "funding_incomplete")}
    out["funding_incomplete"] = bool(leg.get("funding_incomplete"))
    out["positions"] = len(plist)
    out["long_positions"] = ex.get("long_positions")
    out["short_positions"] = ex.get("short_positions")
    out["close_records"] = ex.get("trade_records")
    out["fees_usd"] = ex.get("fees_usd")
    out["funding_pnl_usd"] = ex.get("funding_pnl_usd")
    out["net_pnl_usd"] = ex.get("net_pnl_usd")
    out["partial_close_positions"] = sum(1 for p in plist if int(p.get("closes") or 1) > 1)
    block = leg.get("funding") or {}
    out["funding"] = {k: block.get(k) for k in ("mode", "source", "complete", "unpriced_held_hours")} if block else None
    if leg.get("manifest"):
        cov = leg["manifest"]
        out["manifest"] = {"dataset": cov["dataset"], "candles_sha256": cov["candles_sha256"],
                           "candle_coverage_complete": cov["candle_coverage"]["complete"],
                           "funding_coverage_complete": cov["funding_coverage"].get("complete"),
                           "cost_model": cov["cost_model"]}
        out["rejected_entries"] = ex.get("rejected_entries")
    cv = leg.get("close_validation") or {}
    out["close_validation"] = {k: cv.get(k) for k in ("status", "supported") if k in cv} or None
    return out


def run_leg_task(proto: dict, unit: dict, arm: str) -> tuple:
    import eval_windows as ew
    kind, coin, tf = unit["kind"], unit["coin"], unit["interval"]
    _EQUITY.pop("last", None)
    if unit["section"] == "audit":
        memo = proto["audit"]["memos"][unit["memo"]]
        reg = _WORKER["regs"]["spot"]
        win = tuple(proto["windows"][unit["window"]])
        cost = {}
        if memo["commission_pct"] is not None:
            cost["commission_pct"] = memo["commission_pct"]
        if memo["slippage_pct"] is not None:
            cost["slippage_pct"] = memo["slippage_pct"]
        if arm.startswith("inc:"):
            leg = ew.run_leg(reg, arm[4:], None, proxy_symbol(coin), tf, win, capital=proto["capital_usd"],
                             intrabar_resolution=proto["intrabar_resolution"], funding_mode=memo["funding_mode"],
                             keep_trades=True, **cost)
        else:
            cand = arm_candidate(proto, "audit", int(arm[1:]), coin, tf)
            leg = ew.run_candidate_leg(reg, cand, proxy_symbol(coin), tf, win, proto["capital_usd"],
                                       keep_trades=True, intrabar_resolution=proto["intrabar_resolution"],
                                       funding_mode=memo["funding_mode"], **cost)
        return leg, None
    factor = int(arm[1:])
    cand = arm_candidate(proto, unit["contract"], factor, coin, tf)
    sha = unit_source_sha(proto, unit)
    table = worker_table(sha)
    labels = _WORKER["labels"] if cand.get("regime_label_windows") else None
    if kind == "proxy":
        win = tuple(proto["windows"][unit["window"]])
        reg = FiniteFrameRegistry(_WORKER["regs"]["futures"], table, to_ms(win[0]))
        leg = ew.run_candidate_leg(reg, cand, proxy_symbol(coin), tf, win, proto["capital_usd"], keep_trades=True,
                                   intrabar_resolution=proto["intrabar_resolution"], funding_mode="charge",
                                   warmup_bars=proto["frame"]["warmup_bars"], regime_label_cache=labels)
    else:
        m = _WORKER["manifests"][unit["group"]]
        w = m["windows"][unit["window"]]
        reg = FiniteFrameRegistry(_WORKER["regs"]["futures"], table, int(w["start_ms"]))
        ctx = {"manifest": m, "window": unit["window"], "cost_multiplier": 1.0}
        leg = ew.run_candidate_leg(reg, cand, coin, tf, None, proto["capital_usd"], keep_trades=True,
                                   intrabar_resolution=proto["intrabar_resolution"], manifest_ctx=ctx,
                                   funding_mode="charge", regime_label_cache=labels)
    if reg.calls != 1:
        raise StudyError(f"{unit_id(unit)} {arm}: the finite-frame registry served {reg.calls} calls, expected 1")
    return leg, sha


def unit_id(unit: dict) -> str:
    parts = [unit["section"], unit.get("memo") or unit.get("contract"), unit["group"], unit["interval"],
             unit["coin"], unit["window"]]
    return "_".join(str(p) for p in parts)


def leg_fingerprint(unit: dict, arm: str, proto_sha: str, deps: str, arms: dict, tables_fp: str) -> str:
    return sha256_bytes(json.dumps({"unit": {k: v for k, v in unit.items() if k != "arms"}, "arm": arm,
                                    "protocol": proto_sha, "deps": deps, "arms": arms, "tables": tables_fp},
                                   sort_keys=True).encode())


def run_unit(task) -> dict:
    unit, ckdir, fps = task
    proto = _WORKER["proto"]
    t0 = time.time()
    scored = cached = 0
    for arm in unit["arms"]:
        path = os.path.join(ckdir, unit_id(unit), arm.replace(":", "_") + ".json")
        if checkpoint_fresh(path, fps[arm]) is not None:
            cached += 1
            continue
        t1 = time.time()
        leg, sha = run_leg_task(proto, unit, arm)
        series = _EQUITY.get("last") if leg is not None else None
        eq = daily_equity(series) if not arm.startswith("inc:") else []
        keep_positions = unit["section"] == "perps"
        rng = ({"first_bar": str(series.index[0]), "last_bar": str(series.index[-1]), "bars": int(len(series))}
               if series is not None and len(series) else None)
        rec = {"fingerprint": fps[arm], "unit": unit_id(unit), "arm": arm, "leg": compact_leg(leg),
               "positions": [{k: p[k] for k in ("entry_date", "exit_date", "side", "net_pnl", "fees",
                                                "entry_notional", "closes")}
                             for p in ((leg or {}).get("execution") or {}).get("position_list") or []]
               if keep_positions else [],
               "daily_equity": eq, "effective_range": rng, "table_sha256": sha,
               "seconds": round(time.time() - t1, 2),
               "finished_utc": now_utc()}
        os.makedirs(os.path.dirname(path), exist_ok=True)
        write_json(path, rec)
        scored += 1
    _WORKER["labels"].trim()
    return {"unit": unit_id(unit), "scored": scored, "cached": cached, "seconds": round(time.time() - t0, 1)}


def unit_weight(unit: dict) -> float:
    import pandas as pd
    proto = _WORKER.get("proto") or protocol()
    if unit["kind"] == "proxy":
        s, e = proto["windows"][unit["window"]]
        days = (pd.Timestamp(e) - pd.Timestamp(s)).days
    else:
        days = 120
    per = {"30m": 48, "1h": 24, "4h": 6}[unit["interval"]]
    return days * per * len(unit["arms"])


def tables_fingerprint(work_dir: str) -> str:
    proto = protocol()
    sm = load_json(STUDY_MANIFEST_PATH)
    fps = {}
    for sha, src in table_sources(proto, sm).items():
        fps[sha] = table_fingerprint(sha, proto["frame"]["length_bars"], proto["signal_params"], src["start_ms"])
    return sha256_bytes(json.dumps(fps, sort_keys=True).encode())


def run_units(units: list, phase: str, work_dir: str, base_db: str, workers: int) -> dict:
    proto_sha, deps, arms = protocol_sha256(), engine_fingerprint(), arms_sha256()
    tfp = tables_fingerprint(work_dir)
    ckdir = os.path.join(work_dir, "ckpt")
    tasks, todo = [], []
    for u in sorted(units, key=lambda x: (-unit_weight(x), unit_id(x))):
        fps = {a: leg_fingerprint(u, a, proto_sha, deps, arms, tfp) for a in u["arms"]}
        tasks.append((u, ckdir, fps))
        if any(checkpoint_fresh(os.path.join(ckdir, unit_id(u), a.replace(":", "_") + ".json"), fps[a]) is None
               for a in u["arms"]):
            todo.append((u, ckdir, fps))
    print(f"[{phase}] {len(tasks)} units, {len(todo)} with legs to score, {workers} workers", flush=True)
    started = time.time()
    if todo:
        ctx = multiprocessing.get_context("spawn")
        with ctx.Pool(processes=max(1, min(workers, len(todo))), initializer=worker_init,
                      initargs=(base_db, work_dir)) as pool:
            for i, res in enumerate(pool.imap_unordered(run_unit, todo), 1):
                print(f"[{phase}] {i}/{len(todo)} {res['unit']} scored {res['scored']} cached {res['cached']} "
                      f"{res['seconds']}s (elapsed {round(time.time() - started)}s)", flush=True)
    out = {}
    for u, ck, fps in tasks:
        for a in u["arms"]:
            rec = checkpoint_fresh(os.path.join(ck, unit_id(u), a.replace(":", "_") + ".json"), fps[a])
            if rec is None:
                raise StudyError(f"leg {unit_id(u)} {a} has no fresh checkpoint")
            out[(unit_id(u), a)] = {"unit": u, "rec": rec}
    return out


def prepare_offline(cache_db: str, work_dir: str) -> dict:
    cache_db = refuse_default_db(cache_db)
    frozen = verify_inputs()
    build_cache(cache_db, frozen["study_manifest"])
    block_network()
    install_equity_capture()
    os.makedirs(work_dir, exist_ok=True)
    return {"frozen": frozen, "cache_db": cache_db}


def lock_payload(proto: dict) -> dict:
    return {"protocol_sha256": protocol_sha256(), "study_manifest_sha256": sha256_file(STUDY_MANIFEST_PATH),
            "coverage_sha256": sha256_file(COVERAGE_PATH), "research_config_sha256": sha256_file(RESEARCH_CONFIG_PATH),
            "arms_sha256": arms_sha256(), "dependencies_sha256": dependency_sha256(),
            "driver_sha256": files_digest(DRIVER_FILES)}


def lock() -> None:
    proto = protocol()
    if os.path.exists(LOCK_PATH):
        held = load_json(LOCK_PATH)
        if held["protocol_sha256"] != protocol_sha256():
            raise StudyError(f"protocol.json changed after the lock")
        print(f"protocol already locked at {held['locked_at_utc']}: {held['protocol_sha256']}")
        return
    if any(os.path.exists(p) for p in (SELECTION_PATH, RESULTS_PATH, VERDICT_PATH)):
        raise StudyError("scores exist; the protocol must be locked before any score is written")
    for path in (COVERAGE_PATH, VALIDATION_PATH, RESOLUTION_PATH):
        if not os.path.exists(path):
            raise StudyError(f"{os.path.basename(path)} is missing; the lock needs it")
    verify_inputs()
    build_arms(check=True)
    val = load_json(VALIDATION_PATH)
    rp = val.get("replay") or {}
    for name in ("decomposition", "signal_check_path", "future_invariance", "htf_visibility", "next_bar_fill"):
        if not (rp.get(name) or {}).get("passed"):
            raise StudyError(f"validation.json replay check {name} did not pass; the lock refuses")
    payload = lock_payload(proto)
    payload.update({"locked_at_utc": now_utc(), "source_commit": git_head(),
                    "validation_sha256": sha256_file(VALIDATION_PATH),
                    "replay_summary": {k: {"passed": v.get("passed"), "compared": v.get("compared")}
                                       for k, v in sorted(rp.items()) if isinstance(v, dict)},
                    "note": "written by run_study.py lock before any selection or judged leg"})
    write_json(LOCK_PATH, payload)
    print(f"locked protocol.json {payload['protocol_sha256']}")


def require_lock() -> dict:
    if not os.path.exists(LOCK_PATH):
        raise StudyError("protocol_lock.json is missing; run `run_study.py lock` first")
    held = load_json(LOCK_PATH)
    now = lock_payload(protocol())
    for key in ("protocol_sha256", "study_manifest_sha256", "coverage_sha256", "research_config_sha256",
                "arms_sha256", "dependencies_sha256"):
        if held.get(key) != now.get(key):
            raise StudyError(f"{key} changed after the lock; refusing to score")
    if held.get("validation_sha256") != sha256_file(VALIDATION_PATH):
        raise StudyError("validation.json changed after the lock")
    return held


def perps_proxy_units(proto: dict, cov: dict, window: str, arms_by: dict) -> list:
    units = []
    for contract in contracts(proto):
        for g, spec in sorted(proto["perps"]["groups"].items()):
            if spec["kind"] != "proxy":
                continue
            arms = arms_by(contract, g)
            if not arms:
                continue
            for coin in spec["members"]:
                if not included(cov, g, coin, spec["interval"], window):
                    continue
                units.append({"section": "perps", "contract": contract, "group": g, "kind": "proxy",
                              "coin": coin, "interval": spec["interval"], "window": window,
                              "arms": [f"f{f}" for f in arms]})
    return units


def perps_manifest_units(proto: dict, cov: dict, arms_by: dict) -> list:
    units = []
    for contract in contracts(proto):
        for g, spec in sorted(proto["perps"]["groups"].items()):
            if spec["kind"] == "proxy":
                continue
            proxy_group = f"proxy_{spec['interval']}"
            arms = arms_by(contract, proxy_group)
            for coin in spec["members"]:
                if not included(cov, g, coin, spec["interval"], spec["window"]):
                    continue
                units.append({"section": "perps", "contract": contract, "group": g, "kind": spec["kind"],
                              "coin": coin, "interval": spec["interval"], "window": spec["window"],
                              "arms": [f"f{f}" for f in arms]})
    return units


def audit_units(proto: dict, cov: dict) -> list:
    from eval_windows import INCUMBENTS
    units = []
    arms = [f"f{f}" for f in proto["audit"]["factors"]] + [f"inc:{n}" for n in INCUMBENTS]
    for memo, spec in sorted(proto["audit"]["memos"].items()):
        for w in spec["windows"]:
            for coin, tf in proto["audit"]["datasets"]:
                if not included(cov, "audit", coin, tf, w):
                    continue
                units.append({"section": "audit", "memo": memo, "group": "audit", "kind": "proxy", "coin": coin,
                              "interval": tf, "window": w, "arms": list(arms)})
    return units


def leg_summary_rows(legs: list) -> dict:
    present = [l for l in legs if l is not None]
    if not present:
        return {"datasets": 0}
    return {
        "datasets": len(present),
        "mean_return_pct": round(statistics.mean(l["return_pct"] for l in present), 4),
        "mean_sharpe": round(statistics.mean(l["sharpe"] for l in present), 4),
        "mean_ddadj": round(statistics.mean(l["ddadj"] for l in present), 4),
        "worst_max_dd_pct": round(min(l["max_dd_pct"] for l in present), 4),
        "positions": sum(l["positions"] for l in present),
        "long_positions": sum(l.get("long_positions") or 0 for l in present),
        "short_positions": sum(l.get("short_positions") or 0 for l in present),
        "close_records": sum(l.get("close_records") or 0 for l in present),
        "partial_close_positions": sum(l.get("partial_close_positions") or 0 for l in present),
        "net_pnl_usd": round(sum(l.get("net_pnl_usd") or 0.0 for l in present), 4),
        "fees_usd": round(sum(l.get("fees_usd") or 0.0 for l in present), 4),
        "funding_pnl_usd": round(sum(l.get("funding_pnl_usd") or 0.0 for l in present), 4),
        "funding_incomplete_legs": sum(1 for l in present if l.get("funding_incomplete")),
        "liquidated_legs": sum(1 for l in present if l.get("liquidated")),
        "coverage_complete": all((l.get("manifest") or {}).get("candle_coverage_complete", True)
                                 and (l.get("manifest") or {}).get("funding_coverage_complete", True)
                                 for l in present),
    }


def is_index(res: dict) -> dict:
    out = {}
    for (uid, arm), v in res.items():
        u = v["unit"]
        key = (u.get("contract"), u["group"], u["window"], arm)
        out.setdefault(key, {})[u["coin"]] = v["rec"]["leg"]
    return out


def selection_from(proto: dict, cov: dict, res: dict) -> dict:
    idx = is_index(res)
    w = proto["perps"]["selection_window"]
    rule = proto["selection_rule"]
    screened = list(proto["perps"]["factors_screened"])
    out = {}
    for contract in contracts(proto):
        for g, spec in sorted(proto["perps"]["groups"].items()):
            if spec["kind"] != "proxy":
                continue
            members = sorted(c for c in spec["members"] if included(cov, g, c, spec["interval"], w))
            rows = {}
            for f in [0] + screened:
                legs = idx.get((contract, g, w, f"f{f}"), {})
                rows[f] = leg_summary_rows([legs.get(c) for c in members])
            base = rows[0]

            def better(f):
                r = rows[f]
                return (r.get("datasets") and base.get("datasets") and r["mean_return_pct"] > base["mean_return_pct"]
                        and r["mean_ddadj"] > base["mean_ddadj"])

            shortlist = []
            for f in [x for x in screened if x != 4]:
                pos = screened.index(f)
                nbrs = [screened[i] for i in (pos - 1, pos + 1) if 0 <= i < len(screened)]
                if better(f) and rows[f]["positions"] >= rule["min_positions"] and any(better(n) for n in nbrs):
                    shortlist.append(f)
            pick = None
            if shortlist:
                pick = sorted(shortlist, key=lambda f: (-rows[f]["mean_ddadj"], f))[0]
            out[f"{contract}|{g}"] = {"contract": contract, "group": g, "members_scored": members,
                                      "rows": {f"f{k}": v for k, v in rows.items()},
                                      "improves_on_f0": {f"f{f}": bool(better(f)) for f in screened},
                                      "shortlist": [f"f{f}" for f in shortlist],
                                      "selected_additional": f"f{pick}" if pick is not None else None}
    return out


def select_cmd(cache_db: str, work_dir: str, workers: int) -> None:
    proto = protocol()
    require_lock()
    if os.path.exists(SELECTION_PATH):
        raise StudyError("selection.json exists; the selection is written once and never replaced")
    if os.path.exists(RESULTS_PATH):
        raise StudyError("results.json exists before selection; refusing")
    prep = prepare_offline(cache_db, work_dir)
    cov = load_json(COVERAGE_PATH)
    factors = [0] + list(proto["perps"]["factors_screened"])
    units = perps_proxy_units(proto, cov, proto["perps"]["selection_window"], lambda c, g: factors)
    res = run_units(units, "select", work_dir, prep["cache_db"], workers)
    sel = selection_from(proto, cov, res)
    write_json(SELECTION_PATH, {"study": proto["study"], "protocol_sha256": protocol_sha256(),
                                "lock_sha256": sha256_file(LOCK_PATH), "written_utc": now_utc(),
                                "window": proto["perps"]["selection_window"], "network_calls": len(NETWORK_CALLS),
                                "rule": proto["selection_rule"]["text"], "selection": sel})
    for k, v in sorted(sel.items()):
        print(f"{k}: shortlist {v['shortlist']} selected {v['selected_additional']}")


def require_selection() -> dict:
    if not os.path.exists(SELECTION_PATH):
        raise StudyError("selection.json is missing; run `run_study.py select` before any judged leg")
    sel = load_json(SELECTION_PATH)
    if sel.get("protocol_sha256") != protocol_sha256() or sel.get("lock_sha256") != sha256_file(LOCK_PATH):
        raise StudyError("selection.json was written under another protocol or lock")
    return sel


def judged_factors(sel: dict, contract: str, group: str) -> list:
    pick = sel["selection"][f"{contract}|{group}"]["selected_additional"]
    out = [0, 4]
    if pick:
        out.append(int(pick[1:]))
    return out


def judge_cmd(cache_db: str, work_dir: str, workers: int) -> None:
    proto = protocol()
    require_lock()
    sel = require_selection()
    prep = prepare_offline(cache_db, work_dir)
    probe = network_probe()
    cov = load_json(COVERAGE_PATH)
    log = load_json(RUN_LOG_PATH) if os.path.exists(RUN_LOG_PATH) else {"entries": []}
    log["entries"].append({"command": "judge", "started_utc": now_utc(), "source_commit": git_head(),
                           "workers": workers, "network_probe": probe})
    write_json(RUN_LOG_PATH, log)
    arms_by = lambda c, g: judged_factors(sel, c, g)
    units = []
    for w in proto["perps"]["judged_windows"] + [proto["perps"]["stitched_window"]]:
        units += perps_proxy_units(proto, cov, w, arms_by)
    units += perps_manifest_units(proto, cov, arms_by)
    units += audit_units(proto, cov)
    res = run_units(units, "judge", work_dir, prep["cache_db"], workers)
    sel_res = run_units(perps_proxy_units(proto, cov, proto["perps"]["selection_window"],
                                          lambda c, g: [0] + list(proto["perps"]["factors_screened"])),
                        "select-reread", work_dir, prep["cache_db"], workers)
    res.update(sel_res)
    legs, samples = [], {}
    for (uid, arm), v in sorted(res.items()):
        u, rec = v["unit"], v["rec"]
        row = {k: u.get(k) for k in ("section", "contract", "memo", "group", "kind", "coin", "interval", "window")}
        row.update({"arm": arm, "leg": rec["leg"], "table_sha256": rec.get("table_sha256"),
                    "effective_range": rec.get("effective_range"),
                    "seconds": rec.get("seconds")})
        legs.append(row)
        samples[f"{uid}|{arm}"] = {"positions": rec["positions"], "daily_equity": rec["daily_equity"]}
    write_gz_json(SAMPLES_PATH, samples)
    write_json(RESULTS_PATH, {"study": proto["study"], "protocol_sha256": protocol_sha256(),
                              "lock_sha256": sha256_file(LOCK_PATH), "selection_sha256": sha256_file(SELECTION_PATH),
                              "samples_sha256": sha256_file(SAMPLES_PATH), "source_commit": git_head(),
                              "written_utc": now_utc(), "network_calls": len(NETWORK_CALLS), "legs": legs})
    log = load_json(RUN_LOG_PATH)
    log["entries"][-1].update({"finished_utc": now_utc(), "legs": len(legs), "network_calls": len(NETWORK_CALLS)})
    write_json(RUN_LOG_PATH, log)
    print(f"wrote {len(legs)} legs to results.json and samples.json.gz")


def load_results(path: str = None, samples_path: str = None) -> tuple:
    path = path or RESULTS_PATH
    samples_path = samples_path or SAMPLES_PATH
    res = load_json(path)
    if sha256_file(samples_path) != res["samples_sha256"]:
        raise StudyError("samples.json.gz differs from the digest results.json records")
    return res, read_gz_json(samples_path)


def leg_key(row: dict) -> str:
    u = {"section": row["section"], "memo": row.get("memo"), "contract": row.get("contract"), "group": row["group"],
         "interval": row["interval"], "coin": row["coin"], "window": row["window"]}
    return f"{unit_id(u)}|{row['arm']}"


def index_results(res: dict) -> dict:
    out = {}
    for row in res["legs"]:
        k = (row["section"], row.get("contract") or row.get("memo"), row["group"], row["window"], row["arm"])
        ds = f"{row['coin']} {row['interval']}"
        if ds in out.get(k, {}):
            raise StudyError(f"results.json holds two legs for {k} {ds}")
        out.setdefault(k, {})[ds] = row
    return out


def daily_returns(eq: list, capital: float) -> dict:
    out, prev = {}, capital
    for d, v in eq:
        out[d] = (v / prev - 1.0) if prev else 0.0
        prev = v
    return out


def group_daily(rows_a: dict, rows_b: dict, samples: dict, capital: float) -> dict:
    per_day = {}
    for coin in sorted(set(rows_a) & set(rows_b)):
        ra = daily_returns(samples[leg_key(rows_a[coin])]["daily_equity"], capital)
        rb = daily_returns(samples[leg_key(rows_b[coin])]["daily_equity"], capital)
        for d in sorted(set(ra) & set(rb)):
            per_day.setdefault(d, []).append((ra[d], rb[d]))
    return {d: (statistics.fmean(a for a, _ in v), statistics.fmean(b for _, b in v)) for d, v in per_day.items()}


def block_bootstrap(blocks: list, n: int, seed: int) -> dict:
    flat = [x for b in blocks for x in b]
    if not flat:
        return {"mean": None, "p": 1.0, "blocks": 0, "days": 0}
    mean = statistics.fmean(flat)
    sums = [sum(b) for b in blocks]
    lens = [len(b) for b in blocks]
    rng = random.Random(seed)
    k = len(blocks)
    le = 0
    for _ in range(n):
        tot, cnt = 0.0, 0
        for _j in range(k):
            j = rng.randrange(k)
            tot += sums[j]
            cnt += lens[j]
        if cnt and tot / cnt <= 0:
            le += 1
    return {"mean": mean, "p": (1 + le) / (1 + n), "blocks": k, "days": len(flat)}


def benjamini_hochberg(pvals: dict, alpha: float, family_size: int) -> dict:
    items = sorted(pvals.items(), key=lambda kv: (kv[1], kv[0]))
    m = family_size
    if len(items) > m:
        raise StudyError(f"{len(items)} p-values exceed the family size {m}")
    padded = items + [(f"__unjudged_{i}", 1.0) for i in range(m - len(items))]
    padded.sort(key=lambda kv: (kv[1], kv[0]))
    kmax = 0
    for i, (_, p) in enumerate(padded, 1):
        if p <= alpha * i / m:
            kmax = i
    adj, prev = {}, 1.0
    for i in range(m, 0, -1):
        key, p = padded[i - 1]
        prev = min(prev, p * m / i)
        adj[key] = min(1.0, prev)
    return {k: {"p": p, "q": adj[k], "reject": i < kmax}
            for i, (k, p) in enumerate(padded) if not k.startswith("__unjudged_")}


def window_blocks(daily: dict, start: str, end: str) -> list:
    import pandas as pd
    s = pd.Timestamp(start)
    blocks = {}
    for d, (a, b) in sorted(daily.items()):
        t = pd.Timestamp(d)
        if not (s <= t < pd.Timestamp(end)):
            continue
        blocks.setdefault((t - s).days // 7, []).append(a - b)
    return [blocks[k] for k in sorted(blocks)]


def window_net_blocks(daily: dict, start: str, end: str) -> list:
    import pandas as pd
    s = pd.Timestamp(start)
    blocks = {}
    for d, (a, _b) in sorted(daily.items()):
        t = pd.Timestamp(d)
        if s <= t < pd.Timestamp(end):
            blocks.setdefault((t - s).days // 7, []).append(a)
    return [blocks[k] for k in sorted(blocks)]


def paired_window_verdict(rows_a: dict, rows_b: dict) -> dict:
    coins = sorted(set(rows_a) & set(rows_b))
    la = [rows_a[c]["leg"] for c in coins if rows_a[c]["leg"] is not None and rows_b[c]["leg"] is not None]
    lb = [rows_b[c]["leg"] for c in coins if rows_a[c]["leg"] is not None and rows_b[c]["leg"] is not None]
    if not la:
        return {"verdict": "no data", "datasets": 0}
    sa, sb = leg_summary_rows(la), leg_summary_rows(lb)
    if sa["funding_incomplete_legs"] or sb["funding_incomplete_legs"]:
        v = "funding_incomplete"
    else:
        v = "PASS" if (sa["mean_sharpe"] > sb["mean_sharpe"] and sa["mean_ddadj"] > sb["mean_ddadj"]) else "FAIL"
    return {"verdict": v, "datasets": len(la), "gated": sa, "baseline": sb}


def replay_failed_kinds(val: dict) -> dict:
    rp = val.get("replay") or {}
    finite_ok = all((rp.get(n) or {}).get("passed") for n in
                    ("decomposition", "signal_check_path", "future_invariance", "htf_visibility", "next_bar_fill"))
    full_ok = bool((rp.get("full_frame_causality") or {}).get("passed"))
    return {"finite_frame": finite_ok, "full_frame": full_ok}


def verdict_from(proto: dict, res: dict, samples: dict, sel: dict, val: dict, cov: dict) -> dict:
    idx = index_results(res)
    st = proto["statistics"]
    cap = proto["capital_usd"]
    wins = windows(proto)
    replay = replay_failed_kinds(val)
    members = {}
    for contract in contracts(proto):
        for g, spec in sorted(proto["perps"]["groups"].items()):
            if spec["kind"] != "proxy":
                continue
            picks = {"f4": 4}
            pick = sel["selection"][f"{contract}|{g}"]["selected_additional"]
            if pick:
                picks["selected"] = int(pick[1:])
            for slot, f in sorted(picks.items()):
                key = f"{contract}|{g}|{slot}"
                item = {"contract": contract, "group": g, "slot": slot, "factor": f,
                        "direction": proto["perps"]["contracts"][contract]["direction"], "windows": {},
                        "failures": []}
                blocks, net_blocks, pos, net_usd, usable = [], [], 0, 0.0, set()
                for w in proto["perps"]["judged_windows"]:
                    ra = idx.get(("perps", contract, g, w, f"f{f}"), {})
                    rb = idx.get(("perps", contract, g, w, "f0"), {})
                    pv = paired_window_verdict(ra, rb)
                    item["windows"][w] = pv
                    if pv.get("verdict") == "funding_incomplete":
                        item["failures"].append(f"{w}: funding incomplete")
                    daily = group_daily(ra, rb, samples, cap)
                    s, e = wins[w]
                    blocks += window_blocks(daily, s, e)
                    net_blocks += window_net_blocks(daily, s, e)
                    if pv.get("datasets"):
                        pos += pv["gated"]["positions"]
                        net_usd += pv["gated"]["net_pnl_usd"]
                        usable |= {ra[c]["coin"] for c in set(ra) & set(rb) if ra[c]["leg"] is not None}
                for w in (proto["perps"]["selection_window"], proto["perps"]["stitched_window"]):
                    ra = idx.get(("perps", contract, g, w, f"f{f}"), {})
                    rb = idx.get(("perps", contract, g, w, "f0"), {})
                    item["windows"][w] = paired_window_verdict(ra, rb)
                paired = block_bootstrap(blocks, st["n_resamples"], st["seed"])
                net = block_bootstrap(net_blocks, st["n_resamples"], st["seed"])
                vg = f"venue_{spec['interval']}"
                venue = {}
                for vgroup in proto["perps"]["venue_groups"][spec["interval"]]:
                    vw = proto["perps"]["groups"][vgroup]["window"]
                    ra = idx.get(("perps", contract, vgroup, vw, f"f{f}"), {})
                    rb = idx.get(("perps", contract, vgroup, vw, "f0"), {})
                    venue[vgroup] = paired_window_verdict(ra, rb)
                v = venue.get(vg) or {}
                venue_ok = bool(v.get("datasets")) and v["gated"]["funding_incomplete_legs"] == 0 \
                    and v["gated"]["coverage_complete"] and v["baseline"]["coverage_complete"] \
                    and v["gated"]["net_pnl_usd"] > 0 and v["gated"]["net_pnl_usd"] > v["baseline"]["net_pnl_usd"]
                item.update({"paired": paired, "net": net, "positions": pos, "net_pnl_usd": round(net_usd, 4),
                             "usable_members": sorted(usable), "frozen_members": spec["members"],
                             "venue": venue, "venue_supports": venue_ok})
                if not replay["finite_frame"]:
                    item["failures"].append("finite-frame replay check failed")
                members[key] = item
    pvals = {k: v["paired"]["p"] for k, v in members.items()}
    bh = benjamini_hochberg(pvals, st["alpha"], st["family_size"])
    rule = proto["verdict_rule"]
    for k, item in members.items():
        item["bh"] = bh[k]
        insufficient = []
        if item["positions"] < 100:
            insufficient.append(f"{item['positions']} positions < 100")
        if item["paired"]["blocks"] < 30:
            insufficient.append(f"{item['paired']['blocks']} blocks < 30")
        if len(item["usable_members"]) * 2 < len(item["frozen_members"]):
            insufficient.append(f"{len(item['usable_members'])} of {len(item['frozen_members'])} members usable")
        vg = item["venue"].get(f"venue_{item['group'].split('_')[1]}") or {}
        if not vg.get("datasets"):
            insufficient.append("no venue evidence")
        elif vg["gated"]["funding_incomplete_legs"] or not vg["gated"]["coverage_complete"]:
            insufficient.append("venue funding or candle coverage incomplete")
        positive = (item["net"]["mean"] or 0) > 0 and item["net_pnl_usd"] > 0
        improves = item["bh"]["reject"] and (item["paired"]["mean"] or 0) > 0
        reasons = list(item["failures"]) + insufficient
        if reasons:
            outcome = "inconclusive"
        elif improves and positive and item["venue_supports"]:
            outcome = "opt_in"
        else:
            outcome = "negative"
            if not improves:
                reasons.append(f"paired improvement not significant after correction (p {item['paired']['p']:.4f}, "
                               f"q {item['bh']['q']:.4f}, mean daily {item['paired']['mean']:+.6f})")
            if not positive:
                reasons.append(f"gated net not positive (net PnL {item['net_pnl_usd']:+.2f} USD, mean daily "
                               f"{(item['net']['mean'] or 0):+.6f})")
            if not item["venue_supports"]:
                reasons.append("venue evidence does not support the gate")
        item["outcome"] = outcome
        item["reasons"] = reasons
        if outcome == "opt_in":
            item["suggested_line"] = {"open_strategy": {"name": STRATEGY, "params": {"htf_gate_factor": item["factor"]}},
                                      "scope": f"{item['contract']} {item['group']}"}
    audit = audit_verdicts(proto, idx, samples, replay)
    return {"study": proto["study"], "protocol_sha256": protocol_sha256(),
            "results_sha256": sha256_file(RESULTS_PATH) if os.path.exists(RESULTS_PATH) else None,
            "replay": replay, "family_size": st["family_size"], "members": members, "audit": audit,
            "deployment": {"outcome": "inconclusive", "reason": rule["deployment"]},
            "suggested_lines": {k: v["suggested_line"] for k, v in sorted(members.items()) if v.get("suggested_line")}}


def audit_verdicts(proto: dict, idx: dict, samples: dict, replay: dict) -> dict:
    import eval_windows as ew
    out = {"memos": {}}
    hist = proto["historical"]
    for memo, spec in sorted(proto["audit"]["memos"].items()):
        mrow = {"role": spec["role"], "memo_identity": sha256_bytes(json.dumps(
            {"memo": memo, "commission_pct": spec["commission_pct"], "slippage_pct": spec["slippage_pct"],
             "funding_mode": spec["funding_mode"]}, sort_keys=True).encode())[:16], "windows": {}}
        for w in spec["windows"]:
            incs = {}
            for name in ew.INCUMBENTS:
                for row in idx.get(("audit", memo, "audit", w, f"inc:{name}"), {}).values():
                    incs.setdefault(ew.dataset_key(proxy_symbol(row["coin"]), row["interval"]), {})[name] = row["leg"]
            bars = ew.incumbent_bars(incs)
            per_f = {}
            for f in proto["audit"]["factors"]:
                cand = {}
                for row in idx.get(("audit", memo, "audit", w, f"f{f}"), {}).values():
                    cand[ew.dataset_key(proxy_symbol(row["coin"]), row["interval"])] = row["leg"]
                score = ew.score_candidate(cand, bars)
                per_f[f"f{f}"] = {k: score.get(k) for k in ("verdict", "mean_sharpe", "mean_ddadj", "mean_bar_sharpe",
                                                            "mean_bar_ddadj", "scored_datasets", "traded_datasets",
                                                            "degenerate")}
                legs = [l for l in cand.values() if l is not None]
                per_f[f"f{f}"].update({"positions": sum(l["positions"] for l in legs),
                                       "close_records": sum(l.get("close_records") or 0 for l in legs),
                                       "mean_return_pct": round(statistics.mean(l["return_pct"] for l in legs), 4)
                                       if legs else None,
                                       "worst_max_dd_pct": round(min(l["max_dd_pct"] for l in legs), 4)
                                       if legs else None})
            mrow["windows"][w] = per_f
        out["memos"][memo] = mrow
    comp = {}
    for f in proto["audit"]["factors"]:
        k = str(f)
        comp[f"f{f}"] = {}
        for w in ("is", "oos", "2023", "2024", "2025H1"):
            comp[f"f{f}"][w] = {"982": hist["982"]["verdicts"].get(k, {}).get(w),
                                "1282": hist["1282"]["verdicts"].get(k, {}).get(w),
                                "current": out["memos"]["current"]["windows"].get(w, {}).get(f"f{f}", {}).get("verdict"),
                                "current_funding_off": out["memos"]["current_funding_off"]["windows"].get(w, {}).get(
                                    f"f{f}", {}).get("verdict"),
                                "legacy_cost": out["memos"]["legacy_cost"]["windows"].get(w, {}).get(
                                    f"f{f}", {}).get("verdict")}
    out["comparison"] = comp
    st = proto["statistics"]
    blocks = []
    for w in ("oos", "2024", "2025H1t"):
        ra = idx.get(("audit", "current", "audit", w, "f4"), {})
        rb = idx.get(("audit", "current", "audit", w, "f0"), {})
        daily = group_daily(ra, rb, samples, proto["capital_usd"])
        s, e = windows(proto)[w]
        blocks += window_blocks(daily, s, e)
    out["paired_f4_vs_f0"] = block_bootstrap(blocks, st["n_resamples"], st["seed"])
    out["outcome"] = "inconclusive"
    out["reason"] = ("historical reference only: " + (
        "the full-frame signal path fails the causality check, so its numbers cannot support an opt-in"
        if not replay["full_frame"] else "the audit contract is spot long open-as-close and cannot support a perps "
                                         "opt-in; the windows were examined before"))
    return out


def verdict_cmd() -> None:
    proto = protocol()
    require_lock()
    sel = require_selection()
    res, samples = load_results()
    if res.get("selection_sha256") != sha256_file(SELECTION_PATH) or res.get("lock_sha256") != sha256_file(LOCK_PATH):
        raise StudyError("results.json was scored under another selection or lock")
    out = verdict_from(proto, res, samples, sel, load_json(VALIDATION_PATH), load_json(COVERAGE_PATH))
    write_json(VERDICT_PATH, out)
    for k, v in sorted(out["members"].items()):
        print(f"{k}: {v['outcome']} (p {v['paired']['p']:.4f}, q {v['bh']['q']:.4f}, positions {v['positions']}, "
              f"blocks {v['paired']['blocks']})")
    print(f"audit: {out['audit']['outcome']}")


def replay_samples(proto: dict, df, lo_ms: int, seed_key: str, n: int) -> list:
    import numpy as np
    ts = df["timestamp"].to_numpy()
    length = proto["frame"]["length_bars"]
    lo = max(length - 1, int(np.searchsorted(ts, lo_ms)))
    rng = random.Random(f"{proto['statistics']['seed']}|{seed_key}")
    cands = list(range(lo, len(ts)))
    rng.shuffle(cands)
    return cands[:n * 12]


def _replay_source(task):
    from chart_patterns import _htf_gate_trend, chart_pattern_core
    from mtf_confluence import _resample_htf
    import numpy as np
    import pandas as pd
    proto, sha, src, table_file = task
    length = proto["frame"]["length_bars"]
    params = proto["signal_params"]
    df = candle_frame(repo_path(src["path"]))
    table = SignalTable(read_gz_json(table_file))
    rc = proto["replay_check"]
    half = rc["sample_per_dataset"] // 2
    pool = replay_samples(proto, df, max(src["start_ms"], table.first_ts or 0), sha, rc["sample_per_dataset"])
    nonzero, zero = [], []
    ts_arr = df["timestamp"].to_numpy()
    for p in pool:
        rec = table.by_ts.get(int(ts_arr[p]))
        if rec is None:
            continue
        (nonzero if rec["raw"] != 0 else zero).append(p)
    picks = sorted(nonzero[:half] + zero[:half])
    ch = _check_hyperliquid()
    out = {"decomposition": [], "signal_check_path": [], "future_invariance": [], "htf_visibility": []}
    native = pd.Timedelta(milliseconds=TF_MS[src["dataset"].split()[1]])
    for p in picks:
        frame = df.iloc[p - length + 1:p + 1]
        t = int(ts_arr[p])
        for f in rc["factors"]:
            want = table.signal(t, f, params["htf_gate_mode"])[0]
            direct = int(chart_pattern_core(frame, **{**params, "htf_gate_factor": f})["signal"].iat[-1])
            out["decomposition"].append({"ts": ms_str(t), "factor": f, "table": want, "direct": direct,
                                         "ok": want == direct})
            rows = [[int(r.timestamp), r.open, r.high, r.low, r.close, r.volume] for r in frame.itertuples()]
            shared = ch.build_shared_signal_state(src["dataset"].split()[0], src["dataset"].split()[1],
                                                  df=ch._frame_with_cadence(rows, src["dataset"].split()[1]),
                                                  ohlcv_limit=length, regime_enabled=True, mode="paper")
            got = ch.evaluate_signal_slot(shared, {"strategy": STRATEGY, "mode": "paper",
                                                   "params": {**params, "htf_gate_factor": f}})
            ind = got.get("indicators") or {}
            trend_tab = table.signal(t, f, params["htf_gate_mode"])[1]
            ok = int(got["signal"]) == want and (f <= 1 or int(ind.get("htf_gate_trend", 0)) == trend_tab)
            out["signal_check_path"].append({"ts": ms_str(t), "factor": f, "table": want,
                                             "check_path": int(got["signal"]),
                                             "check_path_trend": ind.get("htf_gate_trend"), "table_trend": trend_tab,
                                             "ok": ok})
        future = df.copy()
        if p + 1 < len(future):
            rng = np.random.default_rng(p)
            scale = rng.uniform(0.5, 1.5, size=len(future) - p - 1)
            for col in ("open", "high", "low", "close"):
                future.iloc[p + 1:, future.columns.get_loc(col)] = future[col].to_numpy()[p + 1:] * scale
        d0 = decide(df.iloc[p - length + 1:p + 1], params)
        d1 = decide(future.iloc[p - length + 1:p + 1], params)
        out["future_invariance"].append({"ts": ms_str(t), "ok": d0 == d1})
        for f in (4, 5):
            htf, visible_at = _resample_htf(frame, f)
            last = frame.index[-1]
            htf_td = native * f
            vis = [(s, v) for s, v in zip(htf.index, visible_at) if v <= last]
            complete = all(s + htf_td <= last + native for s, _ in vis)
            cur = [s for s in htf.index if s <= last < s + htf_td]
            cur_visible = bool(cur) and any(s == cur[0] for s, _ in vis)
            cur_closed = bool(cur) and last == cur[0] + htf_td - native
            ok = complete and (cur_visible == cur_closed)
            out["htf_visibility"].append({"ts": ms_str(t), "factor": f, "visible_buckets": len(vis),
                                          "all_visible_complete": complete, "current_bucket_closed": cur_closed,
                                          "current_bucket_visible": cur_visible, "ok": ok})
    return sha, src, out


_CH = {}


def _check_hyperliquid():
    if "mod" not in _CH:
        import importlib.util
        scripts = repo_path("shared_scripts")
        if scripts not in sys.path:
            sys.path.insert(0, scripts)
        spec = importlib.util.spec_from_file_location("_study2015_check_hyperliquid",
                                                      os.path.join(scripts, "check_hyperliquid.py"))
        mod = importlib.util.module_from_spec(spec)
        spec.loader.exec_module(mod)
        _CH["mod"] = mod
    return _CH["mod"]


def next_bar_fill_check(proto: dict, work_dir: str) -> dict:
    import eval_windows as ew
    import pandas as pd
    from registry_loader import load_registry
    sm = load_json(STUDY_MANIFEST_PATH)
    out = {"legs": [], "compared": 0, "mismatches": 0}
    reg = load_registry("futures")
    for contract, coin, tf in (("ref_long", "BTC", "1h"), ("ref_short", "ETH", "1h"), ("ratchet_both", "SOL", "30m")):
        sha = sm["files"][proxy_rel(coin, tf)]["sha256"]
        table = load_table(work_dir, sha)
        win = tuple(proto["windows"]["is"])
        ff = FiniteFrameRegistry(reg, table, to_ms(win[0]))
        cand = arm_candidate(proto, contract, 4, coin, tf)
        leg = ew.run_candidate_leg(ff, cand, proxy_symbol(coin), tf, win, proto["capital_usd"], keep_trades=True,
                                   intrabar_resolution=proto["intrabar_resolution"], funding_mode="charge",
                                   warmup_bars=proto["frame"]["warmup_bars"])
        src = candle_frame(repo_path(sm["files"][proxy_rel(coin, tf)]["path"]))
        idx = {t: i for i, t in enumerate(src.index)}
        sig_by_ts = {}
        for t in src.index:
            got = table.signal(int(t.value // 1_000_000), 4, "veto")
            if got is not None:
                sig_by_ts[t] = got[0]
        bad, n = [], 0
        for p in leg["execution"]["position_list"]:
            entry = pd.Timestamp(p["entry_date"])
            i = idx.get(entry)
            want = 1 if p["side"] == "long" else -1
            prev_sig = sig_by_ts.get(src.index[i - 1]) if i else None
            n += 1
            if i is None or prev_sig != want:
                bad.append({"entry": str(entry), "side": p["side"], "signal_bar_before": prev_sig})
        out["legs"].append({"contract": contract, "dataset": f"{coin} {tf}", "positions": n,
                            "mismatches": bad[:10], "mismatch_count": len(bad)})
        out["compared"] += n
        out["mismatches"] += len(bad)
    out["passed"] = out["compared"] > 0 and out["mismatches"] == 0
    out["rule"] = ("every opened position's entry bar follows a bar whose finite-frame signal (factor 4) points "
                   "the same way, so a signal at bar N fills at bar N+1")
    return out


def _full_frame_unit(task):
    from chart_patterns import chart_pattern_core
    proto, path, coin, tf, w = task
    rc = proto["replay_check"]
    params = proto["signal_params"]
    s, e = proto["windows"][w]
    src = candle_frame(repo_path(path))
    df = src[(src.index >= s) & (src.index < e)]
    full = chart_pattern_core(df, **{**params, "htf_gate_factor": 0})["signal"].to_numpy()
    nz = [i for i in range(len(full)) if full[i] != 0]
    rng = random.Random(f"{proto['statistics']['seed']}|ff|{coin}|{tf}|{w}")
    pick = sorted(rng.sample(nz, min(len(nz), rc["full_frame_sample_per_dataset_window"])))
    bad = []
    for i in pick:
        cut = int(chart_pattern_core(df.iloc[:i + 1], **{**params, "htf_gate_factor": 0})["signal"].iat[-1])
        if cut != int(full[i]):
            bad.append({"bar": str(df.index[i]), "full_frame": int(full[i]), "cut_frame": cut})
    return {"dataset": f"{coin} {tf}", "window": w, "bars": int(len(df)), "full_frame_signals": len(nz),
            "sampled": len(pick), "reproduced_on_cut_frame": len(pick) - len(bad), "mismatches": len(bad),
            "examples": bad[:5]}


def full_frame_causality(proto: dict, workers: int) -> dict:
    sm = load_json(STUDY_MANIFEST_PATH)
    tasks = [(proto, sm["files"][proxy_rel(coin, tf)]["path"], coin, tf, w)
             for coin, tf in proto["audit"]["datasets"] for w in ("is", "oos", "2024")]
    ctx = multiprocessing.get_context("spawn")
    with ctx.Pool(processes=max(1, min(workers, len(tasks))), initializer=_table_init) as pool:
        rows = list(pool.imap(_full_frame_unit, tasks))
    compared = sum(r["sampled"] for r in rows)
    mismatches = sum(r["mismatches"] for r in rows)
    return {"rows": rows, "compared": compared, "mismatches": mismatches, "passed": compared > 0 and mismatches == 0,
            "windows": ["is", "oos", "2024"],
            "rule": "a full-frame signal at bar N must equal chart_pattern_core on the same window frame cut at bar "
                    "N; a mismatch means the historical path read candles after the decision bar or depended on "
                    "where the window frame ends"}


def finite_vs_full(proto: dict, work_dir: str) -> dict:
    from chart_patterns import chart_pattern_core
    from data_fetcher import load_cached_data
    import pandas as pd
    sm = load_json(STUDY_MANIFEST_PATH)
    params = proto["signal_params"]
    rows = []
    for coin in ("BTC", "ETH", "SOL"):
        tf = "1h"
        table = load_table(work_dir, sm["files"][proxy_rel(coin, tf)]["sha256"])
        for w in ("is", "oos", "2024"):
            s, e = proto["windows"][w]
            df = load_cached_data(proxy_symbol(coin), tf, start_date=s, end_date=e)
            df = df[df.index < pd.Timestamp(e)]
            for f in (0, 4):
                full = chart_pattern_core(df, **{**params, "htf_gate_factor": f})["signal"].to_numpy()
                fin = []
                for t in df.index:
                    got = table.signal(int(t.value // 1_000_000), f, "veto")
                    fin.append(got[0] if got else 0)
                both = sum(1 for a, b in zip(full, fin) if a != 0 and a == b)
                rows.append({"dataset": f"{coin} {tf}", "window": w, "factor": f,
                             "full_frame_signals": int(sum(1 for a in full if a != 0)),
                             "finite_frame_signals": int(sum(1 for b in fin if b != 0)),
                             "same_bar_same_side": both,
                             "full_long": int(sum(1 for a in full if a == 1)),
                             "finite_long": int(sum(1 for b in fin if b == 1))})
    return {"rows": rows, "note": "diagnostic: how far the historical full-frame path and the live-shaped "
                                  "finite frame disagree on the same candles"}


def replay_check_cmd(cache_db: str, work_dir: str, workers: int) -> None:
    proto = protocol()
    if os.path.exists(LOCK_PATH):
        raise StudyError("protocol_lock.json exists; replay-check runs before the lock")
    prep = prepare_offline(cache_db, work_dir)
    sm = prep["frozen"]["study_manifest"]
    tasks = []
    for sha, src in table_sources(proto, sm).items():
        tf = table_path(work_dir, sha)
        load_table(work_dir, sha)
        tasks.append((proto, sha, src, tf))
    ctx = multiprocessing.get_context("spawn")
    per = {}
    with ctx.Pool(processes=max(1, min(workers, len(tasks))), initializer=_table_init) as pool:
        for sha, src, out in pool.imap_unordered(_replay_source, tasks):
            per[sha] = {"source": src, "checks": out}
            print(f"[replay] {src['kind']} {src['dataset']}: " + ", ".join(
                f"{k} {sum(1 for r in v if r['ok'])}/{len(v)}" for k, v in sorted(out.items())), flush=True)
    replay = {}
    for name in ("decomposition", "signal_check_path", "future_invariance", "htf_visibility"):
        rows = [dict(r, source=per[s]["source"]["path"]) for s in sorted(per) for r in per[s]["checks"][name]]
        replay[name] = {"compared": len(rows), "mismatches": [r for r in rows if not r["ok"]][:20],
                        "mismatch_count": sum(1 for r in rows if not r["ok"]),
                        "passed": bool(rows) and all(r["ok"] for r in rows)}
    replay["signal_check_path"]["path"] = ("shared_scripts/check_hyperliquid.py build_shared_signal_state(df=frame, "
                                           "ohlcv_limit=frame length, regime_enabled) + evaluate_signal_slot "
                                           "(prebuilt frame, no adapter, no network)")
    replay["next_bar_fill"] = next_bar_fill_check(proto, work_dir)
    print(f"[replay] next_bar_fill {replay['next_bar_fill']['compared']} positions, "
          f"{replay['next_bar_fill']['mismatches']} mismatches", flush=True)
    replay["full_frame_causality"] = full_frame_causality(proto, workers)
    print(f"[replay] full_frame_causality {replay['full_frame_causality']['compared']} sampled, "
          f"{replay['full_frame_causality']['mismatches']} mismatches", flush=True)
    replay["finite_vs_full_frame"] = finite_vs_full(proto, work_dir)
    write_json(VALIDATION_PATH, {"study": proto["study"], "protocol_sha256": protocol_sha256(),
                                 "study_manifest_sha256": sha256_file(STUDY_MANIFEST_PATH),
                                 "signal_files_sha256": files_digest(SIGNAL_FILES), "written_utc": now_utc(),
                                 "network_probe": network_probe(), "network_calls": len(NETWORK_CALLS),
                                 "frame_length_bars": proto["frame"]["length_bars"], "replay": replay,
                                 "table_sources": {s: {"path": v["source"]["path"], "kind": v["source"]["kind"],
                                                       "dataset": v["source"]["dataset"]}
                                                   for s, v in sorted(per.items())}})
    print(f"wrote {VALIDATION_PATH}")


def render_cmd(check: bool = False) -> None:
    import importlib.util
    spec = importlib.util.spec_from_file_location("_study2015_render", os.path.join(HERE, "render_report.py"))
    mod = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(mod)
    text = mod.render(STUDY_ROOT)
    if check:
        if not os.path.exists(REPORT_PATH) or open(REPORT_PATH).read() != text:
            raise StudyError("REPORT.md differs from a fresh render of the committed artifacts")
        print("REPORT.md matches a fresh render")
        return
    with open(REPORT_PATH, "w") as fh:
        fh.write(text)
    print(f"wrote {REPORT_PATH}")


def main(argv=None) -> int:
    ap = argparse.ArgumentParser(description="chart_pattern_2015 offline study driver (suggest-only)")
    sub = ap.add_subparsers(dest="cmd", required=True)
    sub.add_parser("freeze-inputs")
    sub.add_parser("verify-inputs")
    b = sub.add_parser("build-arms")
    b.add_argument("--check", action="store_true")
    sub.add_parser("coverage")
    t = sub.add_parser("tables")
    t.add_argument("--work-dir", required=True)
    t.add_argument("--workers", type=int, default=1)
    for name in ("replay-check", "select", "judge"):
        q = sub.add_parser(name)
        q.add_argument("--cache-db", required=True)
        q.add_argument("--work-dir", required=True)
        q.add_argument("--workers", type=int, default=1)
    sub.add_parser("lock")
    sub.add_parser("verdict")
    r = sub.add_parser("render")
    r.add_argument("--check", action="store_true")
    args = ap.parse_args(argv)
    if args.cmd == "freeze-inputs":
        freeze_inputs()
    elif args.cmd == "verify-inputs":
        out = verify_inputs()
        print(f"ok: {len(out['study_manifest']['files'])} files and {len(out['manifests'])} manifests match")
    elif args.cmd == "build-arms":
        build_arms(args.check)
    elif args.cmd == "coverage":
        coverage()
    elif args.cmd == "tables":
        build_tables(os.path.abspath(args.work_dir), args.workers)
    elif args.cmd == "replay-check":
        replay_check_cmd(args.cache_db, os.path.abspath(args.work_dir), args.workers)
    elif args.cmd == "lock":
        lock()
    elif args.cmd == "select":
        select_cmd(args.cache_db, os.path.abspath(args.work_dir), args.workers)
    elif args.cmd == "judge":
        judge_cmd(args.cache_db, os.path.abspath(args.work_dir), args.workers)
    elif args.cmd == "verdict":
        verdict_cmd()
    elif args.cmd == "render":
        render_cmd(args.check)
    return 0


if __name__ == "__main__":
    sys.exit(main())

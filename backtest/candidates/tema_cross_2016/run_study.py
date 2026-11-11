#!/usr/bin/env python3

import argparse
import copy
import csv
import gzip
import hashlib
import io
import json
import math
import multiprocessing
import os
import re
import shutil
import socket
import sqlite3
import statistics
import subprocess
import sys
import tempfile
import time
from collections.abc import MutableMapping

HERE = os.path.dirname(os.path.abspath(__file__))
BACKTEST = os.path.abspath(os.path.join(HERE, "..", ".."))
REPO = os.path.abspath(os.path.join(BACKTEST, ".."))
DATA = os.path.join(HERE, "data")
PROXY_DIR = os.path.join(DATA, "proxy")
ARMS_DIR = os.path.join(HERE, "arms")
SOURCE_STUDY = os.path.join(BACKTEST, "candidates", "breakout_2014")
DEPLOYED_PATH = os.path.join(HERE, "deployed_tema_cross_sanitized.json")
PROTOCOL_PATH = os.path.join(HERE, "protocol.json")
LOCK_PATH = os.path.join(HERE, "protocol_lock.json")
STUDY_MANIFEST_PATH = os.path.join(HERE, "study_manifest.json")
COVERAGE_PATH = os.path.join(HERE, "coverage_report.json")
SMOKE_PATH = os.path.join(HERE, "smoke.json")
HTF_PARITY_PATH = os.path.join(HERE, "htf_parity.json")
CACHE_PARITY_PATH = os.path.join(HERE, "cache_parity.json")
TIMING_PATH = os.path.join(HERE, "timing.json")
SELECTION_PATH = os.path.join(HERE, "selection.json")
NOISE_PATH = os.path.join(HERE, "noise_gate.json")
RESULTS_PATH = os.path.join(HERE, "results.json")
SAMPLES_PATH = os.path.join(HERE, "samples.json.gz")
RUN_LOG_PATH = os.path.join(HERE, "run_log.json")
FIDELITY_PATH = os.path.join(HERE, "fidelity.json")
VERDICT_PATH = os.path.join(HERE, "verdict.json")
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
TF_MS = {"30m": 1_800_000, "1h": 3_600_000, "4h": 14_400_000, "1d": 86_400_000}
ENGINE_FILES = (
    "backtest/backtester.py",
    "backtest/eval_windows.py",
    "backtest/htf_trend.py",
    "backtest/regime_label_columns.py",
    "backtest/run_backtest.py",
    "backtest/tune_live.py",
    "backtest/offline_manifest.py",
    "shared_tools/regime.py",
    "shared_tools/atr.py",
    "shared_tools/htf_filter.py",
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
    "backtest/htf_trend.py",
    "shared_tools/htf_filter.py",
)
STATS_FILES = ("backtest/gross_edge_noise.py", "backtest/regime_stats.py", "backtest/exit_policy_ab.py")
SANITIZE_TOP_KEYS = {"_provenance", "live", "paper"}
SANITIZE_SHAPE_KEYS = {"atr_method", "config_version", "default_stop_loss_atr_mult", "regime", "strategies"}
SANITIZE_STRATEGY_KEYS = {
    "allow_no_edge", "args", "capital", "circuit_breaker", "close_strategy", "direction", "htf_filter", "id",
    "initial_capital", "interval_seconds", "leverage", "margin_mode", "margin_per_trade_usd", "max_drawdown_pct",
    "open_strategy", "paused", "platform", "regime_atr_window", "regime_directional_policy",
    "regime_directional_window", "regime_gate_window", "script", "sizing_leverage", "trailing_stop_atr_mult_regime",
    "trailing_stop_min_move_pct", "type",
}
GENERIC_ID = re.compile(r"^tema-cross-(bnb|btc|eth|hype|sol)-(30m|1h)-(paper|live)$")
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
    tmp = path + f".{os.getpid()}.tmp"
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


def write_gz_json(path: str, obj) -> None:
    with open(path, "wb") as fh:
        with gzip.GzipFile(filename="", mode="wb", fileobj=fh, mtime=0) as gz:
            gz.write(json.dumps(obj, sort_keys=True, separators=(",", ":"), default=str).encode())


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


def now_utc() -> str:
    return time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime())


def to_ms(day: str) -> int:
    import pandas as pd
    return int(pd.Timestamp(day, tz="UTC").timestamp() * 1000)


def ms_str(ms) -> str:
    import pandas as pd
    return str(pd.Timestamp(int(ms), unit="ms"))


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
    try:
        held = load_json(path)
    except (OSError, ValueError):
        return None
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


def sanitize_check() -> dict:
    raw = open(DEPLOYED_PATH).read()
    doc = json.loads(raw)
    bad = []
    if set(doc) != SANITIZE_TOP_KEYS:
        bad.append(f"top-level keys {sorted(doc)} != {sorted(SANITIZE_TOP_KEYS)}")
    entries = 0
    for shape in ("live", "paper"):
        block = doc.get(shape) or {}
        extra = sorted(set(block) - SANITIZE_SHAPE_KEYS)
        if extra:
            bad.append(f"{shape}: keys outside the allowlist {extra}")
        for sc in block.get("strategies") or []:
            entries += 1
            extra = sorted(set(sc) - SANITIZE_STRATEGY_KEYS)
            if extra:
                bad.append(f"{shape} {sc.get('id')}: keys outside the allowlist {extra}")
            if not GENERIC_ID.match(str(sc.get("id") or "")):
                bad.append(f"{shape}: id {sc.get('id')!r} is not a generic id")
            if not str(sc.get("id") or "").endswith("-" + shape):
                bad.append(f"{shape}: id {sc.get('id')!r} does not name its shape")
    checks = {
        "40-hex address": re.compile(r"0x[0-9a-fA-F]{40}\b|\b[0-9a-fA-F]{40}\b"),
        "64-hex key": re.compile(r"\b[0-9a-fA-F]{64}\b"),
        "IPv4 address": re.compile(r"\b(?:\d{1,3}\.){3}\d{1,3}\b"),
        "host path": re.compile(r"(/var/|/opt/|/home/|/Users/|/etc/|/root/|\.db\b|\.env\b)"),
        "url or host": re.compile(r"https?://|\b[a-z0-9-]+\.(?:com|net|io|xyz|org)\b", re.I),
        "secret word": re.compile(r"(secret|token|password|private_key|api_key|webhook|channel)", re.I),
    }
    for name, rx in checks.items():
        hit = rx.search(raw)
        if hit:
            bad.append(f"{name}: {hit.group(0)!r}")
    if bad:
        raise SystemExit("sanitize-check failed:\n  " + "\n  ".join(bad))
    out = {"entries": entries, "live": len(doc["live"]["strategies"]), "paper": len(doc["paper"]["strategies"])}
    print(f"ok: {entries} entries (live {out['live']}, paper {out['paper']}), generic ids, no address, IPv4, "
          "host path, URL or secret word")
    return out


def frames(proto: dict) -> dict:
    return proto["frames"]


def frame_datasets(proto: dict, frame: str) -> list:
    fr = proto["frames"][frame]
    return [tuple(d) for d in fr["datasets"]]


def arm_spec(proto: dict, frame: str, arm: str) -> dict:
    return proto["frames"][frame]["arms"][arm]


def arm_datasets(proto: dict, frame: str, arm: str) -> list:
    spec = arm_spec(proto, frame, arm)
    if spec.get("datasets"):
        return [tuple(d) for d in spec["datasets"]]
    return frame_datasets(proto, frame)


def compare_datasets(proto: dict, frame: str, arm: str) -> list:
    spec = arm_spec(proto, frame, arm)
    if spec.get("compare_datasets"):
        return [tuple(d) for d in spec["compare_datasets"]]
    return arm_datasets(proto, frame, arm)


def frame_all_datasets(proto: dict, frame: str) -> list:
    out = set()
    for arm in proto["frames"][frame]["arms"]:
        out |= set(arm_datasets(proto, frame, arm)) | set(compare_datasets(proto, frame, arm))
    return sorted(out)


def leg_arm(proto: dict, frame: str, arm: str) -> str:
    return arm_spec(proto, frame, arm).get("alias") or arm


def config_arm_path(frame: str, arm: str) -> str:
    return os.path.join(ARMS_DIR, frame, f"{arm}.json")


def spot_arm_path(arm: str) -> str:
    return os.path.join(ARMS_DIR, "spot", f"{arm}.json")


def arm_file(proto: dict, frame: str, arm: str) -> str:
    return os.path.join(ARMS_DIR, frame, f"{leg_arm(proto, frame, arm)}.json")


def apply_config_rule(base_cfg: dict, rule: dict) -> dict:
    cfg = copy.deepcopy(base_cfg)
    for key, spec in sorted((rule.get("add_regime_windows") or {}).items()):
        cfg["regime"]["windows"][key] = copy.deepcopy(spec)
    for sc in cfg["strategies"]:
        for key, value in sorted((rule.get("strategy_overrides") or {}).items()):
            if value is None:
                sc.pop(key, None)
            else:
                sc[key] = copy.deepcopy(value)
        if rule.get("params") is not None:
            sc["open_strategy"] = {"name": sc["open_strategy"]["name"], "params": dict(rule["params"])}
        if rule.get("interval"):
            tf = rule["interval"]
            args = list(sc["args"])
            old = args[2]
            args[2] = tf
            sc["args"] = args
            sc["id"] = sc["id"].replace(f"-{old}-", f"-{tf}-")
    return cfg


def resolved_rule(proto: dict, frame: str, arm: str, picks: dict = None) -> dict:
    spec = arm_spec(proto, frame, arm)
    if spec.get("family") == "combo":
        if picks is None:
            raise SystemExit(f"{frame} combo needs the is picks")
        rule = combo_rule(proto, frame, picks)
        if rule is None:
            return None
        return rule
    out = {}
    chain = []
    cur = arm
    while cur:
        chain.append(cur)
        cur = arm_spec(proto, frame, cur).get("base")
    for name in reversed(chain):
        s = arm_spec(proto, frame, name)
        for key in ("add_regime_windows", "strategy_overrides", "candidate_overrides"):
            if s.get(key):
                out.setdefault(key, {}).update(copy.deepcopy(s[key]))
        for key in ("params", "interval"):
            if s.get(key) is not None:
                out[key] = copy.deepcopy(s[key])
    return out


def combo_rule(proto: dict, frame: str, picks: dict):
    fr = proto["frames"][frame]
    spec = fr["arms"]["combo"]
    members = [picks.get(f) for f in spec["families"] if picks.get(f)]
    if len(members) < spec["min_members"]:
        return None
    out = resolved_rule(proto, frame, spec["base"]) if spec.get("base") else {}
    for member in members:
        r = resolved_rule(proto, frame, member)
        for key in ("add_regime_windows", "strategy_overrides", "candidate_overrides"):
            if r.get(key):
                out.setdefault(key, {}).update(r[key])
        for key in ("params", "interval"):
            if r.get(key) is not None:
                out[key] = r[key]
    out["members"] = members
    return out


def deployed_shape(source: str) -> dict:
    doc = load_json(DEPLOYED_PATH)
    shape = copy.deepcopy(doc[source])
    return shape


def build_config_arm(proto: dict, frame: str, arm: str, picks: dict = None):
    fr = proto["frames"][frame]
    rule = resolved_rule(proto, frame, arm, picks)
    if rule is None:
        return None
    return apply_config_rule(deployed_shape(fr["config_source"]), rule)


def spot_regime(proto: dict) -> dict:
    return copy.deepcopy(proto["frames"]["spot"]["regime"])


def build_spot_candidate(proto: dict, arm: str, picks: dict = None):
    import eval_windows as ew
    from regime_label_columns import resolve_feature_windows
    fr = proto["frames"]["spot"]
    rule = resolved_rule(proto, "spot", arm, picks)
    if rule is None:
        return None
    cand = copy.deepcopy(fr["base_candidate"])
    if rule.get("params") is not None:
        cand["params"] = dict(rule["params"])
    over = rule.get("candidate_overrides") or {}
    gate = over.get("regime_gate_window")
    for key, value in sorted(over.items()):
        if key in ("regime_gate_window",):
            continue
        cand[key] = copy.deepcopy(value)
    if gate:
        regime = spot_regime(proto)
        plan = resolve_feature_windows({"regime_gate_window": gate}, regime, label=f"spot {arm}")
        cand.update({"regime_enabled": True, "regime_gate_window": gate,
                     "regime_windows_spec": copy.deepcopy(regime["windows"]),
                     "regime_label_windows": plan,
                     "regime_vocabularies": {k: list(v) for k, v in sorted(plan["vocabularies"].items())}})
        from run_backtest import allowed_regimes_vocabulary_error
        err = allowed_regimes_vocabulary_error(cand.get("allowed_regimes"), plan["classifiers"]["gate"], gate)
        if err:
            raise SystemExit(f"spot {arm}: {err}")
    ew.validate_candidate(copy.deepcopy(cand))
    return cand


def config_strategy_id(cfg: dict, coin: str, tf: str) -> str:
    hits = [sc["id"] for sc in cfg["strategies"] if sc["args"][1] == coin and sc["args"][2] == tf]
    if len(hits) != 1:
        raise SystemExit(f"expected one strategy for {coin} {tf}, found {hits}")
    return hits[0]


def build_arm_candidate(proto: dict, frame: str, arm: str, coin: str, tf: str) -> dict:
    import eval_windows as ew
    if proto["frames"][frame]["builder"] == "candidate":
        return load_json(spot_arm_path(leg_arm(proto, frame, arm)))
    from run_backtest import load_strategy_config
    from tune_live import build_candidate
    path = arm_file(proto, frame, arm)
    cfg = load_json(path)
    sid = config_strategy_id(cfg, coin, tf)
    sc = next(s for s in cfg["strategies"] if s["id"] == sid)
    res = load_strategy_config(path, sid, inject_user_defaults=True)
    open_ref = res["open_strategy"]
    cand = build_candidate(open_ref["name"], open_ref.get("params") or {}, res)
    if sc.get("htf_filter"):
        cand["htf_filter"] = True
    cand["atr_method"] = res["atr_method"]
    ew.validate_candidate(cand)
    return cand


def config_arm_names(proto: dict, frame: str) -> list:
    return sorted(a for a, s in proto["frames"][frame]["arms"].items()
                  if not s.get("alias") and s.get("family") != "combo")


def build_arms(check: bool) -> None:
    proto = protocol()
    want = {}
    for frame, fr in sorted(proto["frames"].items()):
        for arm in config_arm_names(proto, frame):
            if fr["builder"] == "config":
                want[config_arm_path(frame, arm)] = dumps(build_config_arm(proto, frame, arm))
            else:
                want[spot_arm_path(arm)] = dumps(build_spot_candidate(proto, arm))
    bad = []
    for path, text in sorted(want.items()):
        os.makedirs(os.path.dirname(path), exist_ok=True)
        if check:
            if not os.path.exists(path) or open(path).read() != text:
                bad.append(os.path.relpath(path, HERE))
        else:
            with open(path, "w") as fh:
                fh.write(text)
    if bad:
        raise SystemExit(f"arm files differ from the pure build of the deployed copy: {bad}")
    import numpy as np
    import pandas as pd
    from registry_loader import load_registry
    from run_backtest import load_strategy_config
    from regime_label_columns import regime_label_report
    n = 400
    idx = pd.date_range("2025-01-01", periods=n, freq="1h")
    c = 100 * np.exp(np.cumsum(np.random.default_rng(0).normal(0, 0.01, n)))
    for registry in ("spot", "futures"):
        reg = load_registry(registry)
        probe = reg.apply_strategy("tema_cross", pd.DataFrame(
            {"open": c, "high": c * 1.01, "low": c * 0.99, "close": c, "volume": 1.0}, index=idx), {})
        if "atr" in probe.columns:
            raise SystemExit("tema_cross now writes its own atr column; the atr_method harness note is stale")
    report = {}
    for frame, fr in sorted(proto["frames"].items()):
        limits = set()
        for arm in config_arm_names(proto, frame):
            for coin, tf in sorted(set(arm_datasets(proto, frame, arm))):
                cand = build_arm_candidate(proto, frame, arm, coin, tf)
                plan = cand.get("regime_label_windows")
                if fr["builder"] == "config":
                    path = arm_file(proto, frame, arm)
                    sid = config_strategy_id(load_json(path), coin, tf)
                    res = load_strategy_config(path, sid, inject_user_defaults=True)
                    if frame == "paper" and (res["direction"] != "long" or res["regime_directional_certified"]):
                        raise SystemExit(f"paper {arm} {coin} {tf}: expected long and an uncertified policy")
                    if res["atr_method"] != "wilder":
                        raise SystemExit(f"{frame} {arm}: expected atr_method wilder, got {res['atr_method']}")
                if plan:
                    limits.add(int(plan["limit"]))
                row = {"direction": cand["direction"],
                       "close": [r["name"] for r in cand.get("close_strategies") or []],
                       "htf_filter": bool(cand.get("htf_filter")), "atr_method": cand.get("atr_method"),
                       "params": cand.get("params"), "allowed_regimes": cand.get("allowed_regimes"),
                       "gate_window": (plan or {}).get("windows", {}).get("gate"),
                       "label_windows": regime_label_report(plan)["windows"] if plan else None,
                       "label_limit": plan["limit"] if plan else None,
                       "perps_sizing": (cand.get("stop_context") or {}).get("perps_sizing")}
                report[f"{frame} {arm} {coin} {tf}"] = row
        if len(limits) > 1:
            raise SystemExit(f"{frame}: label limit differs across arms {sorted(limits)}")
        if limits and limits != {fr["label_limit"]}:
            raise SystemExit(f"{frame}: label limit {sorted(limits)} != protocol {fr['label_limit']}")
        print(f"{frame}: {len(config_arm_names(proto, frame))} built arms, label limit {sorted(limits) or '-'}")
    for key in sorted(report):
        r = report[key]
        print(f"  {key}: dir {r['direction']} htf {r['htf_filter']} atr {r['atr_method']} params {r['params']} "
              f"gate {r['gate_window']} allowed {r['allowed_regimes']} limit {r['label_limit']}")
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


def copy_frozen() -> None:
    proto = protocol()
    cp = proto["frozen_copy"]
    if os.path.exists(STUDY_MANIFEST_PATH):
        raise SystemExit("study_manifest.json exists; the frozen copy runs once")
    src_manifest = load_json(os.path.join(SOURCE_STUDY, "study_manifest.json"))
    files, copied = {}, {}
    for name, meta in sorted(src_manifest["files"].items()):
        src = os.path.join(SOURCE_STUDY, "data", name)
        dst = os.path.join(DATA, name)
        os.makedirs(os.path.dirname(dst), exist_ok=True)
        shutil.copyfile(src, dst)
        got = sha256_file(dst)
        if got != meta["sha256"]:
            raise SystemExit(f"copied {name} sha256 {got} != source manifest {meta['sha256']}")
        files[name] = dict(meta, copied_from=os.path.relpath(src, REPO))
    for sub in cp["venue_dirs"]:
        for fname in sorted(os.listdir(os.path.join(SOURCE_STUDY, "data", sub))):
            src = os.path.join(SOURCE_STUDY, "data", sub, fname)
            dst = os.path.join(DATA, sub, fname)
            os.makedirs(os.path.dirname(dst), exist_ok=True)
            shutil.copyfile(src, dst)
            copied[f"data/{sub}/{fname}"] = {"source": os.path.relpath(src, REPO), "sha256": sha256_file(dst)}
    for name in sorted(MANIFEST_FILES.values()):
        src = os.path.join(SOURCE_STUDY, name)
        dst = os.path.join(HERE, name)
        shutil.copyfile(src, dst)
        copied[name] = {"source": os.path.relpath(src, REPO), "sha256": sha256_file(dst)}
    write_json(STUDY_MANIFEST_PATH, {
        "study": proto["study"], "pinned_end": proto["proxy"]["pinned_end"],
        "copied_at_utc": now_utc(), "source_commit": git_head(),
        "copy_note": cp["note"], "source_manifest_sha256": sha256_file(os.path.join(SOURCE_STUDY,
                                                                                    "study_manifest.json")),
        "files": dict(sorted(files.items())), "copied_files": dict(sorted(copied.items())),
        "acquisitions": {},
    })
    print(f"copied {len(files)} proxy files and {len(copied)} venue, bridge and manifest files byte for byte")


def acquire_spot_4h(cache_db: str) -> None:
    proto = protocol()
    if os.environ.get("HYPERLIQUID_SECRET_KEY"):
        raise SystemExit("refusing to acquire research data with HYPERLIQUID_SECRET_KEY set")
    cache_db = refuse_default_db(cache_db)
    manifest = load_json(STUDY_MANIFEST_PATH)
    acq = proto["spot_4h_acquisition"]
    targets = [candle_file(c, "4h") for c in acq["coins"]]
    if any(t in manifest["files"] for t in targets) or any(os.path.exists(os.path.join(DATA, t)) for t in targets):
        raise SystemExit("4h candles already exist; acquisition runs once")
    set_cache(cache_db)
    from data_fetcher import get_exchange
    start_ms, end_ms = to_ms(acq["start"]), to_ms(proto["proxy"]["pinned_end"])
    ex = get_exchange(acq["exchange"])
    markets = ex.load_markets()
    for coin in acq["coins"]:
        if proxy_symbol(coin) not in markets:
            raise SystemExit(f"{proxy_symbol(coin)} is not listed on {acq['exchange']}; stop and report")
    for coin in acq["coins"]:
        rows = fetch_candles(ex, proxy_symbol(coin), "4h", start_ms, end_ms)
        if not rows:
            raise SystemExit(f"{coin} 4h: no candles")
        name = candle_file(coin, "4h")
        path = os.path.join(DATA, name)
        write_gz_csv(path, ["timestamp", "open", "high", "low", "close", "volume"], rows)
        ts = [r[0] for r in rows]
        gaps = [[a, b, (b - a) // TF_MS["4h"] - 1] for a, b in zip(ts, ts[1:]) if b - a != TF_MS["4h"]]
        manifest["files"][name] = {
            "kind": "candles", "exchange": acq["exchange"], "symbol": proxy_symbol(coin), "timeframe": "4h",
            "rows": len(rows), "first_ms": ts[0], "last_ms": ts[-1], "first_bar": ms_str(ts[0]),
            "last_bar": ms_str(ts[-1]), "gaps": gaps, "missing_bars": sum(g[2] for g in gaps),
            "sha256": sha256_file(path), "acquired_at_utc": now_utc(),
        }
        print(f"{coin} 4h: {len(rows)} candles {ms_str(ts[0])} .. {ms_str(ts[-1])}, {len(gaps)} gaps", flush=True)
    manifest["files"] = dict(sorted(manifest["files"].items()))
    manifest["acquisitions"]["spot_4h"] = {
        "acquired_at_utc": now_utc(), "source_commit": git_head(), "exchange": acq["exchange"],
        "start": acq["start"], "pinned_end": proto["proxy"]["pinned_end"],
        "method": "ccxt fetch_ohlcv in 1000-bar pages from start; closed bars with opening time >= start and "
                  "< pinned_end; a failed page aborts the acquisition; gaps are listed, never filled",
    }
    write_json(STUDY_MANIFEST_PATH, manifest)


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
        src = meta.get("copied_from")
        if src and os.path.exists(os.path.join(REPO, src)) and sha256_file(os.path.join(REPO, src)) != got:
            raise SystemExit(f"data/{name} differs from its source {src}")
    for name, meta in sorted(manifest["copied_files"].items()):
        got = sha256_file(os.path.join(HERE, name))
        if got != meta["sha256"]:
            raise SystemExit(f"{name} sha256 {got} != manifest {meta['sha256']}")
        src = os.path.join(REPO, meta["source"])
        if os.path.exists(src) and sha256_file(src) != got:
            raise SystemExit(f"{name} differs from its source {meta['source']}")
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


def htf_buckets_before(ts: list, start_ms: int, tf: str, warmup: int) -> int:
    import bisect
    from htf_filter import get_default_htf
    step = TF_MS[tf]
    hstep = TF_MS[get_default_htf(tf)]
    i = bisect.bisect_left(ts, start_ms)
    pre = ts[max(0, i - warmup):i]
    closed = {t // hstep for t in pre if t % hstep == hstep - step}
    return len(closed)


def coverage() -> None:
    proto = protocol()
    frozen = verify_frozen()
    block_network()
    import offline_manifest as om
    windows = study_windows(proto)
    pinned_end = to_ms(proto["proxy"]["pinned_end"])
    files = frozen["proxy"]["files"]
    min_buckets = proto["htf"]["min_closed_buckets"]
    rows = []
    for frame, fr in sorted(proto["frames"].items()):
        warm = fr["warmup_bars"]
        for coin, tf in frame_all_datasets(proto, frame):
            row = {"frame": frame, "dataset": f"{coin} {tf}", "group": "proxy", "coin": coin, "interval": tf,
                   "windows": {}}
            if candle_file(coin, tf) not in files:
                row.update({"status": "excluded", "reason": f"no frozen proxy candles for {coin} {tf}"})
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
                buckets = htf_buckets_before(ts, s, tf, warm)
                item = {"start": start, "end": end, "warmup": pre, "expected_bars": len(expected),
                        "present_bars": present, "missing_bars": len(expected) - present,
                        "unpriced_funding_hours": len(unpriced),
                        "first_unpriced_hour": ms_str(unpriced[0]) if unpriced else None,
                        "htf_closed_buckets_in_warmup": buckets,
                        "htf_rule_ok": buckets >= min_buckets}
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
        if not fr.get("venue"):
            continue
        manifests = frozen["manifests"]
        for coin, tf in fr["venue_datasets"]:
            m = manifests[("venue", tf)]
            row = {"frame": frame, "dataset": f"{coin} {tf}", "group": "venue", "coin": coin, "interval": tf,
                   "windows": {}}
            ds = next((d for d in m["datasets"] if d["coin"] == coin), None)
            if ds is None:
                row.update({"status": "excluded", "reason": "no venue dataset in the manifest"})
                rows.append(row)
                continue
            funding = om.load_funding(ds)
            reasons = []
            for w, win in sorted(m["windows"].items()):
                try:
                    ext, _, cov = om.window_frame(m, ds, w)
                except om.ManifestError as exc:
                    row["windows"][w] = {"status": "excluded", "reason": str(exc)}
                    reasons.append(f"{w}: {exc}")
                    continue
                fcov = om.funding_coverage(funding, win["start_ms"], win["end_ms"])
                ets = [int(t) for t in ext["timestamp"].tolist()]
                buckets = htf_buckets_before(ets, win["start_ms"], tf, m["warmup_bars"])
                why = []
                if not cov["complete"]:
                    why.append(f"{cov['missing_bars']} missing candles")
                if cov["warmup_missing_bars"]:
                    why.append(f"{cov['warmup_missing_bars']} missing warm-up bars")
                if not fcov.get("complete"):
                    why.append(f"funding incomplete ({fcov.get('missing_hours')} missing hours)")
                row["windows"][w] = {"start": win["start"], "end": win["end"], "candles": cov, "funding": fcov,
                                     "htf_closed_buckets_in_warmup": buckets,
                                     "htf_rule_ok": buckets >= min_buckets,
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
                        bext, bwin, bcov = om.window_frame(bm, bds, w)
                        bts = [int(t) for t in bext["timestamp"].tolist()]
                        buckets = htf_buckets_before(bts, bwin["start_ms"], tf, bm["warmup_bars"])
                        ok = not bcov["warmup_missing_bars"]
                        bridge[w] = {"candles": bcov, "status": "included" if ok else "excluded",
                                     "htf_closed_buckets_in_warmup": buckets, "htf_rule_ok": buckets >= min_buckets,
                                     "reason": (f"warm-up complete; {bcov['missing_bars']} missing proxy candles in "
                                                "the window are scored as they are" if ok else
                                                f"{bcov['warmup_missing_bars']} missing warm-up bars")}
                    except om.ManifestError as exc:
                        bridge[w] = {"status": "excluded", "reason": str(exc)}
                row["bridge"] = bridge
            rows.append(row)
    out = {"study": proto["study"], "protocol_sha256": protocol_sha256(),
           "manifest_sha256": sha256_file(STUDY_MANIFEST_PATH), "network_blocked": True, "rows": rows,
           "network_calls": len(NETWORK_CALLS)}
    write_json(COVERAGE_PATH, out)
    for r in rows:
        print(f"{r['frame']:<6} {r['group']:<6} {r['dataset']:<9} {r['status']:<9} {r['reason']}")
    print(f"wrote {COVERAGE_PATH} ({len(rows)} rows)")


def lock() -> None:
    sha = protocol_sha256()
    if os.path.exists(LOCK_PATH):
        held = load_json(LOCK_PATH)
        if held["protocol_sha256"] != sha:
            raise SystemExit(f"protocol.json changed after the lock ({held['protocol_sha256']} -> {sha})")
        print(f"protocol already locked at {held['locked_at_utc']}: {sha}")
        return
    if any(os.path.exists(p) for p in (SELECTION_PATH, NOISE_PATH, RESULTS_PATH)):
        raise SystemExit("scores exist; the protocol must be locked before any score is written")
    for path in (COVERAGE_PATH, SMOKE_PATH, HTF_PARITY_PATH):
        if not os.path.exists(path):
            raise SystemExit(f"{os.path.basename(path)} is missing; the lock needs it")
    parity = load_json(HTF_PARITY_PATH)
    sig = parity.get("signals") or {}
    if not (sig.get("trend_identical") and sig.get("filtered_signal_identical") and sig.get("fetch_calls") == 0):
        raise SystemExit("htf_parity.json signals check did not pass; the lock refuses")
    write_json(LOCK_PATH, {"protocol_sha256": sha, "locked_at_utc": now_utc(), "source_commit": git_head(),
                           "coverage_sha256": sha256_file(COVERAGE_PATH), "smoke_sha256": sha256_file(SMOKE_PATH),
                           "htf_parity_signals": sig,
                           "arms_sha256": arms_sha256(protocol()),
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
    now = arms_sha256(protocol())
    for k, v in held["arms_sha256"].items():
        if now.get(k) != v:
            raise SystemExit(f"arm file {k} changed after the lock")
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


def arms_sha256(proto: dict) -> dict:
    out = {}
    for frame in sorted(proto["frames"]):
        sub = os.path.join(ARMS_DIR, frame)
        if not os.path.isdir(sub):
            continue
        for name in sorted(os.listdir(sub)):
            if name.endswith(".json"):
                out[f"{frame}/{name}"] = sha256_file(os.path.join(sub, name))
    return out


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
    proto = protocol()
    _WORKER["proto"] = proto
    _WORKER["regs"] = {fr["registry"]: load_registry(fr["registry"]) for fr in proto["frames"].values()}
    _WORKER["labels"] = DiskLabelCache(label_cache_root(work_dir))
    _WORKER["manifests"] = load_manifests(verify=True)


def leg_cost_kwargs(proto: dict, kind: str) -> dict:
    if kind == "gross":
        return {"commission_pct": 0.0, "slippage_pct": 0.0}
    if kind == "stress":
        from backtester import DEFAULT_SLIPPAGE_PCT
        return {"slippage_pct": proto["costs"]["cached_stress_slippage_multiplier"] * DEFAULT_SLIPPAGE_PCT}
    return {}


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


def unit_id(unit: dict) -> str:
    return "_".join([unit["frame"], unit["group"], unit["interval"], unit["coin"], unit["window"]])


def leg_id(arm: str, kind: str) -> str:
    return f"{arm}__{kind}"


def run_one_leg(proto, unit, arm, kind, labels):
    import eval_windows as ew
    frame = unit["frame"]
    fr = proto["frames"][frame]
    reg = _WORKER["regs"][fr["registry"]]
    coin, tf = unit["coin"], unit["interval"]
    cand = build_arm_candidate(proto, frame, arm, coin, tf)
    cache = labels if cand.get("regime_label_windows") else None
    if unit["group"] == "proxy":
        return ew.run_candidate_leg(reg, cand, proxy_symbol(coin), tf, tuple(unit["range"]), proto["capital_usd"],
                                    keep_trades=True, funding_mode=proto["funding_mode"],
                                    warmup_bars=fr["warmup_bars"], regime_label_cache=cache,
                                    **leg_cost_kwargs(proto, kind))
    m = _WORKER["manifests"][(unit["group"], tf)]
    ctx = {"manifest": m, "window": unit["window"], "cost_multiplier": float(kind[1:])}
    return ew.run_candidate_leg(reg, cand, coin, tf, None, proto["capital_usd"], keep_trades=True,
                                manifest_ctx=ctx, funding_mode=proto["funding_mode"], regime_label_cache=cache)


def run_unit(task) -> dict:
    unit, ckdir, fingerprints = task
    proto, labels = _WORKER["proto"], _WORKER["labels"]
    t0 = time.time()
    labels.hits = labels.misses = 0
    scored, cached = 0, 0
    for arm, kind in unit["legs"]:
        path = os.path.join(ckdir, unit_id(unit), leg_id(arm, kind) + ".json")
        fp = fingerprints[leg_id(arm, kind)]
        if checkpoint_fresh(path, fp) is not None:
            cached += 1
            continue
        t1 = time.time()
        leg = run_one_leg(proto, unit, arm, kind, labels)
        rng = None
        if unit["group"] == "proxy":
            rng = proxy_effective_range(proxy_symbol(unit["coin"]), unit["interval"], tuple(unit["range"]),
                                        proto["frames"][unit["frame"]]["warmup_bars"])
        elif leg is not None:
            cov = leg["manifest"]["candle_coverage"]
            rng = {"scored_first_bar": cov["first_bar"], "scored_last_bar": cov["last_bar"],
                   "scored_bars": cov["present_bars"], "warmup_bars": cov["warmup_bars"]}
        os.makedirs(os.path.dirname(path), exist_ok=True)
        write_json(path, {"fingerprint": fp, "unit": unit_id(unit), "arm": arm, "kind": kind, "leg": leg,
                          "range": rng, "seconds": round(time.time() - t1, 1), "pid": os.getpid(),
                          "finished_utc": now_utc()})
        scored += 1
    out = {"unit": unit_id(unit), "scored": scored, "cached": cached, "seconds": round(time.time() - t0, 1),
           "label_cache": {"hits": labels.hits, "misses": labels.misses}}
    labels.trim()
    return out


def leg_fingerprint(unit: dict, arm: str, kind: str, proto_sha: str, eng: dict, arms: dict, lab: str,
                    proto: dict) -> str:
    frame = unit["frame"]
    path = arm_file(proto, frame, arm)
    key = os.path.relpath(path, ARMS_DIR)
    return sha256_bytes(json.dumps({"unit": {k: v for k, v in unit.items() if k != "legs"}, "arm": arm,
                                    "kind": kind, "arm_sha256": arms.get(key), "protocol": proto_sha,
                                    "engine": eng, "labeller": lab}, sort_keys=True).encode())


def unit_weight(unit: dict) -> float:
    import pandas as pd
    if unit["group"] == "proxy":
        s, e = unit["range"]
        days = (pd.Timestamp(e) - pd.Timestamp(s)).days
    else:
        days = 100
    per = {"30m": 48, "1h": 24, "4h": 6}[unit["interval"]]
    frame_cost = {"spot": 0.3, "paper": 1.0, "live": 1.0}[unit["frame"]]
    return days * per * frame_cost + len(unit["legs"])


def run_units(units: list, phase: str, work_dir: str, base_db: str, workers: int) -> dict:
    proto = protocol()
    proto_sha, eng, arms, lab = protocol_sha256(), engine_sha256(), arms_sha256(proto), labeller_sha256()
    ckdir = os.path.join(work_dir, "ckpt")
    tasks = []
    for u in sorted(units, key=unit_weight, reverse=True):
        fps = {leg_id(a, k): leg_fingerprint(u, a, k, proto_sha, eng, arms, lab, proto) for a, k in u["legs"]}
        tasks.append((u, ckdir, fps))
    todo = []
    for u, ck, fps in tasks:
        if any(checkpoint_fresh(os.path.join(ck, unit_id(u), leg_id(a, k) + ".json"), fps[leg_id(a, k)]) is None
               for a, k in u["legs"]):
            todo.append((u, ck, fps))
    print(f"[{phase}] {len(tasks)} units, {len(todo)} with legs to score, {workers} workers", flush=True)
    started = time.time()
    if todo:
        ctx = multiprocessing.get_context("spawn")
        with ctx.Pool(processes=min(workers, len(todo)), initializer=worker_init,
                      initargs=(base_db, work_dir)) as pool:
            for i, res in enumerate(pool.imap_unordered(run_unit, todo), 1):
                print(f"[{phase}] {i}/{len(todo)} {res['unit']} scored {res['scored']} cached {res['cached']} "
                      f"{res['seconds']}s labels {res['label_cache']} (elapsed {round(time.time() - started)}s)",
                      flush=True)
    out = {}
    for u, ck, fps in tasks:
        for a, k in u["legs"]:
            path = os.path.join(ck, unit_id(u), leg_id(a, k) + ".json")
            rec = checkpoint_fresh(path, fps[leg_id(a, k)])
            if rec is None:
                raise SystemExit(f"leg {unit_id(u)} {a} {k} has no fresh checkpoint")
            out[(u["frame"], u["group"], u["window"], a, k, u["coin"], u["interval"])] = rec
    return out


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


def included_proxy(cov: dict, frame: str, window: str) -> list:
    out = []
    for r in cov["rows"]:
        if (r["frame"] == frame and r["group"] == "proxy" and r["status"] == "included"
                and r["windows"][window]["status"] == "included"):
            out.append((r["coin"], r["interval"]))
    return out


def included_manifest(cov: dict, frame: str, group: str, window: str, tf: str) -> list:
    out = []
    for r in cov["rows"]:
        if r["frame"] != frame or r["group"] != "venue" or r["interval"] != tf or r["status"] != "included":
            continue
        if group == "venue":
            if window in r["windows"] and r["windows"][window]["status"] == "included":
                out.append(r["coin"])
        elif r.get("bridge") and r["bridge"].get(window, {}).get("status") == "included":
            out.append(r["coin"])
    return out


def proxy_units(proto: dict, cov: dict, frame: str, window: str, arms: list, kinds: list) -> list:
    wins = study_windows(proto)
    want = {}
    for arm in arms:
        la = leg_arm(proto, frame, arm)
        for ds in arm_datasets(proto, frame, arm):
            want.setdefault(ds, set()).add(la)
    units = []
    ok = set(included_proxy(cov, frame, window))
    for (coin, tf), las in sorted(want.items()):
        if (coin, tf) not in ok:
            continue
        legs = [(a, k) for a in sorted(las) for k in kinds]
        units.append({"frame": frame, "group": "proxy", "coin": coin, "interval": tf, "window": window,
                      "range": list(wins[window]), "legs": legs})
    return units


def manifest_units(proto: dict, cov: dict, frame: str, arms: list) -> list:
    fr = proto["frames"][frame]
    if not fr.get("venue"):
        return []
    costs = [f"x{c:g}" for c in sorted(proto["venue"]["cost_multipliers"].values())]
    want = {}
    for arm in arms:
        la = leg_arm(proto, frame, arm)
        for coin, tf in arm_datasets(proto, frame, arm):
            want.setdefault((coin, tf), set()).add(la)
    units = []
    for (coin, tf), las in sorted(want.items()):
        for group in ("venue", "bridge"):
            for w in proto["venue"]["windows"][tf]:
                if coin not in included_manifest(cov, frame, group, w, tf):
                    continue
                units.append({"frame": frame, "group": group, "coin": coin, "interval": tf, "window": w,
                              "legs": [(a, k) for a in sorted(las) for k in costs]})
    for u in units:
        if u["coin"] not in [c for c, _ in fr["venue_datasets"]]:
            raise SystemExit(f"{frame}: venue unit for {u['coin']} outside the venue dataset list")
    return units


def selection_arms(proto: dict, frame: str) -> list:
    return sorted(a for a, s in proto["frames"][frame]["arms"].items() if s.get("family") != "combo")


def select_units(proto: dict, cov: dict, arms_by_frame: dict) -> list:
    w = proto["proxy"]["selection_window"]
    units = []
    for frame in sorted(arms_by_frame):
        units += proxy_units(proto, cov, frame, w, arms_by_frame[frame], ["net", "gross", "stress"])
    return units


def position_clusters(spans: list) -> int:
    import pandas as pd
    spans = sorted((pd.Timestamp(s), pd.Timestamp(e)) for s, e in spans)
    count, end = 0, None
    for s, e in spans:
        if end is None or s > end:
            count += 1
            end = e
        else:
            end = max(end, e)
    return count


def cluster_ids(spans: list) -> dict:
    import pandas as pd
    order = sorted(range(len(spans)), key=lambda i: (pd.Timestamp(spans[i][0]), pd.Timestamp(spans[i][1])))
    out, cid, end = {}, -1, None
    for i in order:
        s, e = pd.Timestamp(spans[i][0]), pd.Timestamp(spans[i][1])
        if end is None or s > end:
            cid += 1
            end = e
        else:
            end = max(end, e)
        out[i] = cid
    return out


def leg_funding(leg: dict) -> dict:
    block = leg.get("funding") or {}
    if not block and leg.get("manifest"):
        cov = leg["manifest"]["funding_coverage"]
        return {"source": "manifest", "complete": cov.get("complete"), "missing_hours": cov.get("missing_hours")}
    return {"source": block.get("source"), "complete": block.get("complete"), "mode": block.get("mode"),
            "unpriced_held_hours": block.get("unpriced_held_hours")}


def dataset_key(coin: str, tf: str, group: str) -> str:
    return f"{proxy_symbol(coin)} {tf}" if group == "proxy" else f"{coin} {tf}"


def samples_of(leg: dict, ds: str, window: str, field: str) -> list:
    if leg is None:
        return []
    positions = (leg.get("execution") or {}).get("position_list") or []
    span = {}
    for p in positions:
        span.setdefault(p["entry_date"], (p["entry_date"], p["exit_date"], p["side"]))
    out = []
    for s in leg.get("trade_samples") or []:
        sp = span.get(s["entry_date"], (s["entry_date"], s["entry_date"], "long"))
        out.append({"dataset": ds, "window": window, "entry": s["entry_date"], "exit": sp[1], "side": sp[2],
                    "value": float(s[field])})
    return out


def summarize(proto: dict, legs: dict, gross_legs: dict = None, stress_legs: dict = None) -> dict:
    capital = proto["capital_usd"]
    present = {k: v for k, v in legs.items() if v is not None}
    out = {"datasets": len(present), "datasets_requested": len(legs)}
    if not present:
        out.update({"trades": 0, "clusters": 0, "positions": 0, "mean_ddadj": None, "mean_return_pct": None,
                    "mean_sharpe": None, "worst_max_dd_pct": None, "net_per_trade_pct": None,
                    "gross_per_trade_pct": None, "stress_per_trade_pct": None, "trades_per_year": None,
                    "funding_incomplete_legs": 0, "traded_datasets": 0, "per_dataset": {}})
        return out
    per, spans, net_vals = {}, [], []
    span_days = 0.0
    for ds, leg in sorted(present.items()):
        ex = leg.get("execution") or {}
        plist = ex.get("position_list") or []
        item = {"return_pct": leg["return_pct"], "sharpe": leg["sharpe"], "max_dd_pct": leg["max_dd_pct"],
                "ddadj": leg["ddadj"], "trades": leg["trades"], "span_days": leg.get("span_days"),
                "positions": ex.get("positions"), "long_positions": ex.get("long_positions"),
                "short_positions": ex.get("short_positions"), "fees_usd": ex.get("fees_usd"),
                "funding_pnl_usd": ex.get("funding_pnl_usd"), "net_pnl_usd": ex.get("net_pnl_usd"),
                "fee_drag_pct": round(100.0 * (ex.get("fees_usd") or 0.0) / capital, 4),
                "funding_drag_pct": round(-100.0 * (ex.get("funding_pnl_usd") or 0.0) / capital, 4),
                "liquidated": bool(leg.get("liquidated")), "funding_incomplete": bool(leg.get("funding_incomplete")),
                "funding": leg_funding(leg), "htf_filter": leg.get("htf_filter"),
                "position_spans": [[str(p["entry_date"]), str(p["exit_date"])] for p in plist]}
        if leg.get("manifest"):
            item["candle_coverage_complete"] = leg["manifest"]["candle_coverage"]["complete"]
            item["funding_coverage_complete"] = leg["manifest"]["funding_coverage"].get("complete")
            item["rejected_entries"] = ex.get("rejected_entries")
        net = [float(s["pnl_pct_net"]) for s in leg.get("trade_samples") or []]
        item["net_per_trade_pct"] = round(statistics.fmean(net), 6) if net else None
        net_vals += net
        span_days += float(leg.get("span_days") or 0.0)
        per[ds] = item
        spans += item["position_spans"]
    vals = list(per.values())
    trades = sum(v["trades"] for v in vals)

    def pooled(src, field):
        if not src:
            return None, 0
        xs = []
        for ds in sorted(present):
            xs += [float(s[field]) for s in (src.get(ds) or {}).get("trade_samples") or []]
        return (round(statistics.fmean(xs), 6) if xs else None), len(xs)

    gross, gross_n = pooled(gross_legs, "pnl_pct")
    stress, stress_n = pooled(stress_legs, "pnl_pct_net")
    netm = round(statistics.fmean(net_vals), 6) if net_vals else None
    out.update({
        "mean_return_pct": round(statistics.mean(v["return_pct"] for v in vals), 4),
        "mean_sharpe": round(statistics.mean(v["sharpe"] for v in vals), 4),
        "mean_ddadj": round(statistics.mean(v["ddadj"] for v in vals), 4),
        "worst_max_dd_pct": round(min(v["max_dd_pct"] for v in vals), 4),
        "trades": trades, "traded_datasets": sum(1 for v in vals if v["trades"] > 0),
        "positions": len(spans),
        "long_positions": sum(v["long_positions"] or 0 for v in vals),
        "short_positions": sum(v["short_positions"] or 0 for v in vals),
        "clusters": position_clusters(spans),
        "span_days": round(span_days, 2),
        "trades_per_year": round(trades / (span_days / 365.25), 2) if span_days else None,
        "net_per_trade_pct": netm, "gross_per_trade_pct": gross, "gross_trades": gross_n,
        "stress_per_trade_pct": stress, "stress_trades": stress_n,
        "cost_drag_per_trade_pct": round(gross - netm, 6) if gross is not None and netm is not None else None,
        "fee_drag_pct": round(statistics.mean(v["fee_drag_pct"] for v in vals), 4),
        "funding_drag_pct": round(statistics.mean(v["funding_drag_pct"] for v in vals), 4),
        "net_pnl_usd": round(sum(v["net_pnl_usd"] or 0.0 for v in vals), 4),
        "liquidated_legs": sum(1 for v in vals if v["liquidated"]),
        "funding_incomplete_legs": sum(1 for v in vals if v["funding_incomplete"]),
        "funding_fetched_legs": sum(1 for v in vals if v["funding"]["source"] == "fetched"),
        "coverage_complete": all(v.get("candle_coverage_complete", True) and v.get("funding_coverage_complete", True)
                                 for v in vals),
        "htf_blocked": {"long": sum((v["htf_filter"] or {}).get("long_blocked", 0) for v in vals),
                        "short": sum((v["htf_filter"] or {}).get("short_blocked", 0) for v in vals)},
        "per_dataset": per,
    })
    return out


def brief(summary: dict) -> dict:
    return {k: v for k, v in summary.items() if k != "per_dataset"}


def legs_for(idx: dict, frame: str, group: str, window: str, arm_name: str, kind: str, datasets: list) -> dict:
    out = {}
    for coin, tf in datasets:
        rec = idx.get((frame, group, window, arm_name, kind, coin, tf))
        if rec is None:
            continue
        out[dataset_key(coin, tf, group)] = rec["leg"]
    return out


def arm_summary(proto: dict, idx: dict, frame: str, group: str, window: str, arm: str, datasets: list,
                kinds=("net", "gross", "stress")) -> dict:
    la = leg_arm(proto, frame, arm)
    if group == "proxy":
        net = legs_for(idx, frame, group, window, la, "net", datasets)
        gross = legs_for(idx, frame, group, window, la, "gross", datasets) if "gross" in kinds else None
        stress = legs_for(idx, frame, group, window, la, "stress", datasets) if "stress" in kinds else None
        return summarize(proto, net, gross, stress)
    return summarize(proto, legs_for(idx, frame, group, window, la, kinds[0], datasets))


def selection_from(proto: dict, cov: dict, idx: dict, include_combo: bool = False) -> dict:
    rule = proto["selection_rule"]
    w = proto["proxy"]["selection_window"]
    out = {}
    for frame in sorted(proto["frames"]):
        fr = proto["frames"][frame]
        ok = set(included_proxy(cov, frame, w))
        arms = [a for a in sorted(fr["arms"]) if include_combo or fr["arms"][a]["family"] != "combo"]
        summ, checks, shortlist = {}, {}, []
        for arm in arms:
            if arm not in idx.get("__built__", {}).get(frame, set(arms)):
                continue
            ds = [d for d in arm_datasets(proto, frame, arm) if d in ok]
            summ[arm] = brief(arm_summary(proto, idx, frame, "proxy", w, arm, ds))
        for arm in arms:
            spec = fr["arms"][arm]
            if spec["family"] in ("control", "reference") or arm not in summ:
                continue
            ctrl = spec["control"]
            cds = [d for d in compare_datasets(proto, frame, arm) if d in ok]
            c_s = brief(arm_summary(proto, idx, frame, "proxy", w, ctrl, cds))
            s = summ[arm]
            n_ds = len([d for d in arm_datasets(proto, frame, arm) if d in ok])
            c = {
                "beats_control_ddadj": s["mean_ddadj"] is not None and c_s["mean_ddadj"] is not None
                and s["mean_ddadj"] > c_s["mean_ddadj"],
                "min_trades": s["trades"] >= rule["min_is_trades"],
                "min_clusters": s["clusters"] >= rule["min_is_clusters"],
                "trades_on_half_the_datasets": n_ds > 0 and s["traded_datasets"] >= math.ceil(n_ds / 2),
                "no_funding_incomplete": s["funding_incomplete_legs"] == 0,
            }
            if spec["family"] == "period":
                grid = fr["period_grid"]
                i = grid.index(arm)
                nbrs = [grid[j] for j in (i - 1, i + 1) if 0 <= j < len(grid)]
                c["plateau_neighbours_beat_control"] = all(
                    summ.get(n, {}).get("mean_ddadj") is not None
                    and summ[n]["mean_ddadj"] > c_s["mean_ddadj"] for n in nbrs)
            checks[arm] = {"checks": c, "control": ctrl, "control_mean_ddadj": c_s["mean_ddadj"],
                           "control_datasets": [f"{a} {b}" for a, b in cds]}
            if all(c.values()):
                shortlist.append(arm)
        picks = {}
        for fam in fr["families"]:
            cands = [a for a in shortlist if fr["arms"][a]["family"] == fam]
            picks[fam] = max(cands, key=lambda a: (summ[a]["mean_ddadj"], a)) if cands else None
        out[frame] = {"arms": summ, "checks": checks, "shortlist": shortlist, "picks": picks}
    return out


def write_combo_arms(proto: dict, picks_by_frame: dict) -> dict:
    built = {}
    for frame, picks in sorted(picks_by_frame.items()):
        fr = proto["frames"][frame]
        if fr["builder"] == "config":
            cfg = build_config_arm(proto, frame, "combo", picks)
            path = config_arm_path(frame, "combo")
        else:
            cfg = build_spot_candidate(proto, "combo", picks)
            path = spot_arm_path("combo")
        if cfg is None:
            built[frame] = None
            if os.path.exists(path):
                os.remove(path)
            continue
        with open(path, "w") as fh:
            fh.write(dumps(cfg))
        built[frame] = {"path": os.path.relpath(path, HERE), "sha256": sha256_file(path),
                        "members": combo_rule(proto, frame, picks)["members"]}
    return built


def select_cmd(cache_db: str, work_dir: str, workers: int) -> None:
    proto = protocol()
    held = require_lock()
    cov = load_json(COVERAGE_PATH)
    if os.path.exists(SELECTION_PATH):
        raise SystemExit("selection.json exists; the selection is written once")
    frozen, cache_db = prepare_offline(cache_db, work_dir)
    probe = network_probe()
    arms_a = {f: [a for a in selection_arms(proto, f)] for f in proto["frames"]}
    idx = run_units(select_units(proto, cov, arms_a), "select", work_dir, cache_db, workers)
    first = selection_from(proto, cov, idx)
    combos = write_combo_arms(proto, {f: s["picks"] for f, s in first.items()})
    combo_frames = {f: ["combo"] for f, c in combos.items() if c}
    if combo_frames:
        idx.update(run_units(select_units(proto, cov, combo_frames), "select-combo", work_dir, cache_db, workers))
    built = {f: set(selection_arms(proto, f)) | ({"combo"} if combos.get(f) else set()) for f in proto["frames"]}
    idx["__built__"] = built
    sel = selection_from(proto, cov, idx, include_combo=True)
    for f in sel:
        sel[f]["picks_before_combo"] = first[f]["picks"]
        sel[f]["combo"] = combos.get(f)
        sel[f]["judged_arms"] = sorted(set(a for a in sel[f]["picks"].values() if a)
                                       | ({"combo"} if "combo" in sel[f]["shortlist"] else set()))
    del idx["__built__"]
    ranges = {}
    for (frame, group, window, arm, kind, coin, tf), rec in idx.items():
        ranges[f"{frame} {group} {window} {coin} {tf}"] = rec["range"]
    out = {"frames": sel, "window": proto["proxy"]["selection_window"], "protocol_sha256": protocol_sha256(),
           "protocol_lock": held, "coverage_sha256": sha256_file(COVERAGE_PATH), "source_commit": git_head(),
           "engine_sha256": engine_sha256(), "arms_sha256": arms_sha256(proto), "network_probe": probe,
           "network_blocked_while_scoring": True, "written_at_utc": now_utc(),
           "effective_ranges": dict(sorted(ranges.items())),
           "leg_seconds": round(sum(r["seconds"] for r in idx.values()), 1)}
    write_json(SELECTION_PATH, out)
    for f, s in sorted(sel.items()):
        print(f"{f}: shortlist {s['shortlist']} picks {s['picks']} combo {s['combo']}")
    print(f"wrote {SELECTION_PATH}")


def require_selection() -> dict:
    if not os.path.exists(SELECTION_PATH):
        raise SystemExit("selection.json is missing; run `run_study.py select` first")
    sel = load_json(SELECTION_PATH)
    if sel["protocol_sha256"] != protocol_sha256():
        raise SystemExit("selection.json was written under a different protocol.json")
    for k, v in sel["arms_sha256"].items():
        if arms_sha256(protocol()).get(k) != v:
            raise SystemExit(f"arm file {k} changed after the selection")
    return sel


def noise_units(proto: dict, cov: dict) -> list:
    units = []
    for frame, fr in sorted(proto["frames"].items()):
        for w in proto["proxy"]["noise_windows"]:
            units += proxy_units(proto, cov, frame, w, fr["noise_baselines"], ["gross"])
    return units


def pooled_values(proto, idx, frame, arm, windows, kind, field, datasets_by_window):
    la = leg_arm(proto, frame, arm)
    rows = []
    for w in windows:
        for coin, tf in datasets_by_window[w]:
            rec = idx.get((frame, "proxy", w, la, kind, coin, tf))
            if rec is None:
                continue
            rows += samples_of(rec["leg"], dataset_key(coin, tf, "proxy"), w, field)
    return rows


def cluster_stats(rows: list, n_resamples: int, seed: int) -> dict:
    from gross_edge_noise import analyze_sample, cluster_sign_flip_permutation
    vals = [r["value"] for r in rows]
    cids = cluster_ids([(r["entry"], r["exit"]) for r in rows])
    clusters = [cids[i] for i in range(len(rows))]
    base = analyze_sample(vals, n_resamples=n_resamples, seed=seed)
    cl = cluster_sign_flip_permutation(vals, clusters, n_resamples=n_resamples, seed=seed)
    return {"n": len(vals), "summary": base["summary"], "permutation": base["permutation"],
            "cluster_permutation": cl, "bootstrap": base["bootstrap"], "verdict": base["verdict"],
            "sign_test": base["sign_test"], "wilcoxon": base["wilcoxon"]}


def noise_gate_cmd(cache_db: str, work_dir: str, workers: int) -> None:
    proto = protocol()
    held = require_lock()
    sel = require_selection()
    cov = load_json(COVERAGE_PATH)
    if os.path.exists(NOISE_PATH):
        raise SystemExit("noise_gate.json exists; the noise gate is written once")
    frozen, cache_db = prepare_offline(cache_db, work_dir)
    probe = network_probe()
    idx = run_units(noise_units(proto, cov), "noise", work_dir, cache_db, workers)
    out = {}
    for frame, fr in sorted(proto["frames"].items()):
        dsw = {w: included_proxy(cov, frame, w) for w in proto["proxy"]["noise_windows"]}
        for arm in fr["noise_baselines"]:
            dsw_arm = {w: [d for d in v if d in arm_datasets(proto, frame, arm)] for w, v in dsw.items()}
            rows = pooled_values(proto, idx, frame, arm, proto["proxy"]["noise_windows"], "gross", "pnl_pct", dsw_arm)
            st = cluster_stats(rows, proto["statistics"]["n_resamples"], proto["statistics"]["seed"])
            st["datasets_by_window"] = {w: [f"{c} {t}" for c, t in v] for w, v in dsw_arm.items()}
            out[f"{frame}:{arm}"] = st
            print(f"noise {frame}:{arm}: n {st['n']} mean {st['summary']['mean']} p {st['permutation']['p_value']} "
                  f"cluster p {st['cluster_permutation']['p_value']} verdict {st['verdict']}")
    write_json(NOISE_PATH, {"protocol_sha256": protocol_sha256(), "selection_sha256": sha256_file(SELECTION_PATH),
                            "protocol_lock": held, "source_commit": git_head(), "network_probe": probe,
                            "windows": proto["proxy"]["noise_windows"],
                            "field": "per-trade pnl_pct of the zero-cost (gross) leg", "baselines": out,
                            "written_at_utc": now_utc()})
    print(f"wrote {NOISE_PATH}")


def judged_arms(proto: dict, sel: dict, frame: str) -> list:
    fr = proto["frames"][frame]
    s = sel["frames"][frame]
    arms = set(fr["noise_baselines"]) | {a for a, sp in fr["arms"].items() if sp["family"] in ("control", "reference")}
    arms |= set(s["shortlist"]) | set(s["judged_arms"])
    for a in list(arms):
        ctrl = fr["arms"][a].get("control")
        if ctrl:
            arms.add(ctrl)
    return sorted(arms)


def judge_units(proto: dict, cov: dict, sel: dict) -> list:
    units = []
    for frame in sorted(proto["frames"]):
        arms = judged_arms(proto, sel, frame)
        for w in proto["proxy"]["judged_windows"]:
            units += proxy_units(proto, cov, frame, w, arms, ["net", "gross", "stress"])
        units += manifest_units(proto, cov, frame, arms)
    return units


def judge_cmd(cache_db: str, work_dir: str, workers: int) -> None:
    proto = protocol()
    held = require_lock()
    sel = require_selection()
    if not os.path.exists(NOISE_PATH):
        raise SystemExit("noise_gate.json is missing; run `run_study.py noise-gate` first")
    cov = load_json(COVERAGE_PATH)
    frozen, cache_db = prepare_offline(cache_db, work_dir)
    probe = network_probe()
    log = load_json(RUN_LOG_PATH) if os.path.exists(RUN_LOG_PATH) else {"runs": [], "starts": []}
    started = now_utc()
    log["starts"].append({"started_utc": started, "protocol_sha256": protocol_sha256(), "source_commit": git_head()})
    write_json(RUN_LOG_PATH, log)
    t0 = time.time()
    units = judge_units(proto, cov, sel)
    idx = run_units(units, "judge", work_dir, cache_db, workers)
    sel_arms = {f: [a for a in judged_arms(proto, sel, f)] for f in proto["frames"]}
    idx.update(run_units(select_units(proto, cov, sel_arms), "select-reuse", work_dir, cache_db, workers))
    noise_idx = run_units(noise_units(proto, cov), "noise-reuse", work_dir, cache_db, workers)
    idx.update(noise_idx)
    proxy, manifest, samples = {}, {}, {}
    for frame in sorted(proto["frames"]):
        fr = proto["frames"][frame]
        arms = judged_arms(proto, sel, frame)
        proxy[frame] = {}
        for w in [proto["proxy"]["selection_window"]] + proto["proxy"]["judged_windows"]:
            ok = set(included_proxy(cov, frame, w))
            proxy[frame][w] = {}
            for arm in arms:
                ds = [d for d in arm_datasets(proto, frame, arm) if d in ok]
                proxy[frame][w][arm] = arm_summary(proto, idx, frame, "proxy", w, arm, ds)
        for arm in arms:
            for kind, field in (("net", "pnl_pct_net"), ("gross", "pnl_pct"), ("stress", "pnl_pct_net")):
                rows = []
                for w in proto["proxy"]["pooled_windows"] + [proto["proxy"]["selection_window"]]:
                    ok = set(included_proxy(cov, frame, w))
                    for coin, tf in arm_datasets(proto, frame, arm):
                        if (coin, tf) not in ok:
                            continue
                        rec = idx.get((frame, "proxy", w, leg_arm(proto, frame, arm), kind, coin, tf))
                        if rec is not None:
                            rows += samples_of(rec["leg"], dataset_key(coin, tf, "proxy"), w, field)
                samples[f"{frame}:{arm}:{kind}"] = rows
        if fr.get("venue"):
            manifest[frame] = {}
            for group in ("venue", "bridge"):
                manifest[frame][group] = {}
                for label, cost in sorted(proto["venue"]["cost_multipliers"].items()):
                    kind = f"x{cost:g}"
                    for tf in sorted({tf for _, tf in fr["venue_datasets"]}):
                        for w in proto["venue"]["windows"][tf]:
                            key = f"{tf} {w} {label}"
                            manifest[frame][group][key] = {}
                            coins = included_manifest(cov, frame, group, w, tf)
                            for arm in arms:
                                ds = [(c, t) for c, t in arm_datasets(proto, frame, arm)
                                      if t == tf and c in coins]
                                if not ds:
                                    continue
                                manifest[frame][group][key][arm] = arm_summary(
                                    proto, idx, frame, group, w, arm, sorted(set(ds)), kinds=(kind,))
    fetched = sum(1 for k, rec in idx.items() if rec["leg"] is not None
                  and leg_funding(rec["leg"]).get("source") == "fetched")
    ranges = {}
    for (frame, group, window, arm, kind, coin, tf), rec in idx.items():
        ranges[f"{frame} {group} {window} {coin} {tf}"] = rec["range"]
    if fetched:
        raise SystemExit(f"{fetched} legs fetched funding while scoring")
    write_gz_json(SAMPLES_PATH, {"protocol_sha256": protocol_sha256(), "samples": samples})
    out = {
        "study": proto["study"], "issue": proto["issue"], "protocol_sha256": protocol_sha256(), "protocol_lock": held,
        "selection_sha256": sha256_file(SELECTION_PATH), "noise_gate_sha256": sha256_file(NOISE_PATH),
        "coverage_sha256": sha256_file(COVERAGE_PATH), "manifest_sha256": sha256_file(STUDY_MANIFEST_PATH),
        "offline_manifests_sha256": {n: sha256_file(os.path.join(HERE, n)) for n in sorted(MANIFEST_FILES.values())},
        "source_commit": git_head(), "engine_sha256": engine_sha256(), "arms_sha256": arms_sha256(proto),
        "network_blocked_while_scoring": True, "network_probe": probe,
        "funding_fetched_legs": fetched, "capital_usd": proto["capital_usd"],
        "judged_arms": {f: judged_arms(proto, sel, f) for f in proto["frames"]},
        "proxy": proxy, "manifest": manifest, "samples_sha256": sha256_file(SAMPLES_PATH),
        "effective_ranges": dict(sorted(ranges.items())),
        "runtime_seconds": round(time.time() - t0, 1),
    }
    write_json(RESULTS_PATH, out)
    log["runs"].append({"started_utc": started, "finished_utc": now_utc(), "protocol_sha256": protocol_sha256(),
                        "selection_sha256": sha256_file(SELECTION_PATH), "source_commit": git_head(),
                        "units": len(units)})
    write_json(RUN_LOG_PATH, log)
    print(f"wrote {RESULTS_PATH} and {SAMPLES_PATH}; judge runs logged: {len(log['runs'])}")


def composite_windows_read(proto: dict, frame: str, arm: str) -> set:
    fr = proto["frames"][frame]
    if fr["builder"] == "candidate":
        cand = load_json(spot_arm_path(leg_arm(proto, frame, arm)))
        plan = cand.get("regime_label_windows")
    else:
        coin, tf = arm_datasets(proto, frame, arm)[0]
        cand = build_arm_candidate(proto, frame, arm, coin, tf)
        plan = cand.get("regime_label_windows")
    if not plan:
        return set()
    out = set()
    for field in ("gate", "directional", "atr"):
        if plan["classifiers"][field] == "composite":
            out.add((plan["windows"][field], "composite", arm))
        elif field == "gate" and cand.get("allowed_regimes"):
            out.add((plan["windows"][field], "adx", arm))
    return out


def arm_uses_htf(proto: dict, frame: str, arm: str) -> bool:
    fr = proto["frames"][frame]
    coin, tf = arm_datasets(proto, frame, arm)[0]
    if fr["builder"] == "candidate":
        return bool(load_json(spot_arm_path(leg_arm(proto, frame, arm))).get("htf_filter"))
    return bool(build_arm_candidate(proto, frame, arm, coin, tf).get("htf_filter"))


def fidelity_units(proto: dict, cov: dict, sel: dict) -> list:
    units = []
    for frame, fr in sorted(proto["frames"].items()):
        readers = list(fr["noise_baselines"]) + list(sel["frames"][frame]["judged_arms"])
        windows = set()
        htf_ds = set()
        for arm in readers:
            windows |= composite_windows_read(proto, frame, arm)
            if arm_uses_htf(proto, frame, arm):
                htf_ds |= set(arm_datasets(proto, frame, arm))
        by_name = {}
        for name, clf, arm in sorted(windows):
            by_name.setdefault((name, clf), []).append(arm)

        def reader(arms, coin, tf):
            return next((a for a in arms if (coin, tf) in arm_datasets(proto, frame, a)), None)
        for w in proto["fidelity"]["proxy_windows"]:
            for coin, tf in included_proxy(cov, frame, w):
                for (name, clf), arms in sorted(by_name.items()):
                    arm = reader(arms, coin, tf)
                    if arm:
                        units.append({"frame": frame, "group": "proxy", "coin": coin, "interval": tf, "window": w,
                                      "kind": clf, "regime_window": name, "arm": arm})
                if (coin, tf) in htf_ds:
                    units.append({"frame": frame, "group": "proxy", "coin": coin, "interval": tf, "window": w,
                                  "kind": "htf", "regime_window": "htf", "arm": None})
        if fr.get("venue"):
            for coin, tf in fr["venue_datasets"]:
                for w in proto["fidelity"]["venue_windows"][tf]:
                    if coin not in included_manifest(cov, frame, "venue", w, tf):
                        continue
                    for (name, clf), arms in sorted(by_name.items()):
                        arm = reader(arms, coin, tf)
                        if arm:
                            units.append({"frame": frame, "group": "venue", "coin": coin, "interval": tf,
                                          "window": w, "kind": clf, "regime_window": name, "arm": arm})
    return units


def fidelity_frames(proto: dict, unit: dict, manifests: dict):
    import pandas as pd
    warm = proto["frames"][unit["frame"]]["warmup_bars"]
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


def regime_spec_for(proto: dict, frame: str, arm: str, name: str) -> tuple:
    from regime import _normalize_spec
    coin, tf = arm_datasets(proto, frame, arm)[0]
    if proto["frames"][frame]["builder"] == "candidate":
        cand = load_json(spot_arm_path(leg_arm(proto, frame, arm)))
    else:
        cand = build_arm_candidate(proto, frame, arm, coin, tf)
    plan = cand["regime_label_windows"]
    return _normalize_spec(copy.deepcopy(plan["windows_spec"][name])), plan


def engine_labels(plan: dict, name: str, ext, labels: MutableMapping):
    from regime_label_columns import label_cache_key, label_frame_sha256
    spec = (plan.get("windows_spec") or {}).get(name)
    key = label_cache_key(label_frame_sha256(ext), int(plan["limit"]), int(plan.get("period") or 14),
                          float(plan.get("adx_threshold") or 20.0), name, spec)
    if key not in labels:
        return None
    return labels[key][""]


def htf_live_rule_row(proto: dict, unit: dict, window, ext) -> dict:
    import pandas as pd
    from htf_filter import HTF_EMA_PERIOD, get_default_htf, htf_trend_filter
    from htf_trend import derive_htf_candles, htf_trend_from_candles
    tf = unit["interval"]
    htf_df, cov = derive_htf_candles(ext, tf)
    frozen = htf_trend_from_candles(htf_df, ext.index)
    hstep = pd.Timedelta(milliseconds=TF_MS[get_default_htf(tf)])
    depth = HTF_EMA_PERIOD + 10
    n = equal = 0
    mismatches = []
    first = window.index[0]
    closes = htf_df.index + hstep
    for i in range(len(htf_df)):
        t = closes[i]
        if t < first or t > window.index[-1]:
            continue
        if i + 1 < depth:
            continue
        hist = htf_df.iloc[i + 1 - depth:i + 1]
        info = htf_trend_filter("X", tf, lambda s, h, k: hist.tail(k))
        if info["htf_status"] != "ok":
            continue
        pos = ext.index.searchsorted(t)
        if pos >= len(ext.index) or ext.index[pos] != t:
            continue
        n += 1
        if int(info["htf_trend"]) == int(frozen.iloc[pos]):
            equal += 1
        elif len(mismatches) < 10:
            mismatches.append({"bar": str(t), "live_rule": int(info["htf_trend"]), "frozen": int(frozen.iloc[pos])})
    agreement = round(equal / n, 6) if n else None
    return {"n": n, "agreement": agreement, "mismatches": mismatches, "derived": cov, "fetch_depth": depth,
            "passes": bool(n >= proto["fidelity"]["min_agreement_bars"]
                           and agreement >= proto["fidelity"]["agreement_threshold"])}


def run_fidelity_unit(task):
    unit, path, fingerprint = task
    if checkpoint_fresh(path, fingerprint) is not None:
        return {"unit": unit, "status": "cached"}
    import numpy as np
    from regime import compute_regime, compute_regime_composite
    from regime_bounded_window_validate import validate_frames
    proto = protocol()
    t0 = time.time()
    manifests = load_manifests(verify=True)
    window, ext = fidelity_frames(proto, unit, manifests)
    eval_start = len(ext) - len(window)
    fr = proto["frames"][unit["frame"]]
    out = {"unit": unit, "eval_bars": len(window), "first_bar": str(window.index[0]),
           "last_bar": str(window.index[-1])}
    if unit["kind"] == "htf":
        out.update(htf_live_rule_row(proto, unit, window, ext))
    else:
        spec, plan = regime_spec_for(proto, unit["frame"], unit["arm"], unit["regime_window"])
        lookback = int(plan["limit"])
        engine = engine_labels(plan, unit["regime_window"], ext, DiskLabelCache(label_cache_root(_FID["work_dir"])))
        check = {"engine_labels_found": engine is not None, "sampled": 0, "equal": 0, "mismatches": []}
        if unit["kind"] == "composite":
            rep = validate_frames(window, ext, eval_start, None, incumbent_period=spec["period"],
                                  thresholds=spec["thresholds"], lookback=lookback,
                                  agreement_threshold=proto["fidelity"]["agreement_threshold"],
                                  min_agreement_bars=proto["fidelity"]["min_agreement_bars"])
            drift = rep["handrule"]["label_drift"]
            out.update({"n": drift["n"], "agreement": drift["agreement"], "disagreements": drift["disagreements"],
                        "transitions": drift["transitions"], "period": spec["period"],
                        "thresholds": spec["thresholds"]})
            if engine is not None:
                every = proto["fidelity"]["engine_check_every"]
                for i in range(eval_start, len(ext), every):
                    if i + 1 < lookback or not engine[i]:
                        continue
                    w = ext.iloc[i - lookback + 1:i + 1]
                    lab = compute_regime_composite(w, period=spec["period"], thresholds=spec["thresholds"],
                                                   tail=1)["regime"]
                    got = str(lab.iloc[-1]) if len(lab) else ""
                    check["sampled"] += 1
                    if got == engine[i]:
                        check["equal"] += 1
                    elif len(check["mismatches"]) < 10:
                        check["mismatches"].append({"bar": str(ext.index[i]), "engine": engine[i], "validator": got})
        else:
            period = int(spec["period"])
            full = compute_regime(window, period=period, adx_threshold=float(plan.get("adx_threshold") or 20.0))
            full_lab = full["regime"].astype(str).to_numpy()
            n = equal = 0
            if engine is not None:
                eng = np.asarray(engine[eval_start:], dtype=object)
                for a, b in zip(full_lab, eng):
                    if not a or not b or a in ("nan", "None"):
                        continue
                    n += 1
                    equal += int(a == b)
            out.update({"n": n, "agreement": round(equal / n, 6) if n else None, "period": period,
                        "comparison": "full-window compute_regime on the scored window against the engine's bounded "
                                      "labels"})
        out["passes"] = bool(out["n"] and out["n"] >= proto["fidelity"]["min_agreement_bars"]
                             and out["agreement"] is not None
                             and out["agreement"] >= proto["fidelity"]["agreement_threshold"])
        out["engine_check"] = check
        out["lookback"] = lookback
    out["seconds"] = round(time.time() - t0, 1)
    out["fingerprint"] = fingerprint
    write_json(path, out)
    return {"unit": unit, "status": "scored", "seconds": out["seconds"]}


_FID = {}


def fidelity_init(work_dir: str) -> None:
    for p in (os.path.join(REPO, "shared_tools"), BACKTEST):
        if p not in sys.path:
            sys.path.insert(0, p)
    block_network()
    _FID["work_dir"] = work_dir


def fidelity_name(u: dict) -> str:
    return f"{u['frame']}_{u['group']}_{u['interval']}_{u['coin']}_{u['window']}_{u['kind']}_{u['regime_window']}.json"


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
    ckdir = os.path.join(work_dir, "ckpt_fidelity")
    os.makedirs(ckdir, exist_ok=True)
    proto_sha, eng, arms = protocol_sha256(), engine_sha256(), arms_sha256(proto)
    lab, val = labeller_sha256(), files_digest(VALIDATOR_FILES)
    tasks = []
    for u in units:
        fp = sha256_bytes(json.dumps({"unit": u, "protocol": proto_sha, "engine": eng, "arms": arms,
                                      "labeller": lab, "validator": val}, sort_keys=True).encode())
        tasks.append((u, os.path.join(ckdir, fidelity_name(u)), fp))
    tasks.sort(key=lambda t: (t[0]["kind"] == "htf", -{"30m": 2, "1h": 1, "4h": 0}[t[0]["interval"]]))
    print(f"[fidelity] {len(tasks)} rows, {workers} workers", flush=True)
    ctx = multiprocessing.get_context("spawn")
    with ctx.Pool(processes=workers, initializer=fidelity_init, initargs=(work_dir,)) as pool:
        for i, res in enumerate(pool.imap_unordered(run_fidelity_unit, tasks), 1):
            u = res["unit"]
            print(f"[fidelity] {i}/{len(tasks)} {fidelity_name(u)} {res['status']} {res.get('seconds', '')}",
                  flush=True)
    rows = [{k: v for k, v in load_json(t[1]).items() if k != "fingerprint"} for t in tasks]
    rows.sort(key=lambda r: fidelity_name(r["unit"]))
    checked = [r for r in rows if r.get("engine_check")]
    bad = [r for r in checked if r["engine_check"]["engine_labels_found"]
           and r["engine_check"]["equal"] != r["engine_check"]["sampled"]]
    missing = [r for r in checked if not r["engine_check"]["engine_labels_found"]]
    write_json(FIDELITY_PATH, {
        "protocol_sha256": protocol_sha256(), "selection_sha256": sha256_file(SELECTION_PATH),
        "results_sha256": sha256_file(RESULTS_PATH), "source_commit": git_head(), "network_blocked": True,
        "rows": rows,
        "engine_check": {"rows_checked": len(checked) - len(missing), "rows_without_engine_labels": len(missing),
                         "rows_with_mismatch": len(bad),
                         "sampled_bars": sum(r["engine_check"]["sampled"] for r in checked)}})
    if bad:
        raise SystemExit(f"engine cached labels disagree with the validator's bounded labels in {len(bad)} rows")
    print(f"wrote {FIDELITY_PATH}: {len(rows)} rows, {sum(1 for r in rows if r['passes'])} pass")


def frame_exclusions(proto: dict, frame: str, res: dict, fid: dict, cov: dict) -> dict:
    excluded = {}
    fr = proto["frames"][frame]
    for r in cov["rows"]:
        if r["frame"] == frame and r["group"] == "proxy" and r["status"] != "included":
            excluded.setdefault((r["coin"], r["interval"]), []).append(f"coverage: {r['reason']}")
    for w, by_arm in res["proxy"][frame].items():
        for arm, s in by_arm.items():
            for ds, item in s["per_dataset"].items():
                if item["funding_incomplete"]:
                    sym, tf = ds.split(" ")
                    excluded.setdefault((sym.split("/")[0], tf), []).append(f"{arm} {w}: funding_incomplete")
    for row in fid["rows"]:
        u = row["unit"]
        if u["frame"] == frame and not row["passes"]:
            excluded.setdefault((u["coin"], u["interval"]), []).append(
                f"failing {u['group']} {u['kind']} fidelity row {u['regime_window']} {u['window']}")
    return {k: sorted(set(v)) for k, v in excluded.items() if k in set(frame_all_datasets(proto, frame))}


def pooled_rows(samples: dict, frame: str, arm: str, kind: str, windows: list, keep: set) -> list:
    rows = samples.get(f"{frame}:{arm}:{kind}") or []
    out = []
    for r in rows:
        if r["window"] not in windows:
            continue
        sym, tf = r["dataset"].split(" ")
        if (sym.split("/")[0], tf) in keep:
            out.append(r)
    return out


def mean_block(per: dict, keys: list):
    vals = [per[k] for k in keys if k in per and per[k] is not None]
    if not vals:
        return None
    spans = [s for v in vals for s in v["position_spans"]]
    return {"mean_sharpe": statistics.mean(v["sharpe"] for v in vals),
            "mean_ddadj": statistics.mean(v["ddadj"] for v in vals),
            "mean_return_pct": statistics.mean(v["return_pct"] for v in vals),
            "worst_max_dd_pct": min(v["max_dd_pct"] for v in vals),
            "complete": all(v.get("candle_coverage_complete", True) and v.get("funding_coverage_complete", True)
                            and not v["funding_incomplete"] for v in vals),
            "datasets": len(vals), "clusters": position_clusters(spans)}


def venue_verdict(proto: dict, res: dict, frame: str, arm: str, keep: set) -> dict:
    rule = proto["venue_rule"]
    fr = proto["frames"][frame]
    ctrl = fr["arms"][arm]["control"]
    w = proto["venue"]["verdict_window"]
    a_keys = [f"{c} {t}" for c, t in arm_datasets(proto, frame, arm) if (c, t) in keep]
    c_keys = [f"{c} {t}" for c, t in compare_datasets(proto, frame, arm) if (c, t) in keep]
    out = {"window": w, "arm_datasets": a_keys, "control": ctrl, "control_datasets": c_keys}
    blocks = {}
    for label in sorted(proto["venue"]["cost_multipliers"]):
        a_parts, c_parts = {}, {}
        for tf in sorted({k.split(" ")[1] for k in a_keys + c_keys}):
            key = f"{tf} {w} {label}"
            summ = res["manifest"][frame]["venue"].get(key, {})
            a_parts.update((summ.get(arm) or {}).get("per_dataset") or {})
            c_parts.update((summ.get(ctrl) or {}).get("per_dataset") or {})
        blocks[label] = {"arm": mean_block(a_parts, a_keys), "control": mean_block(c_parts, c_keys)}
    a1, c1 = blocks["base"]["arm"], blocks["base"]["control"]
    a2, c2 = blocks["stress"]["arm"], blocks["stress"]["control"]
    out.update({"arm_x1": a1, "control_x1": c1, "arm_x2": a2, "control_x2": c2})
    present = all(x is not None for x in (a1, c1, a2, c2)) and a1["datasets"] == len(a_keys) \
        and c1["datasets"] == len(c_keys)
    if not a_keys or not present or not all(x["complete"] for x in (a1, c1, a2, c2)):
        out.update({"outcome": "inconclusive", "reason": "venue coverage incomplete or missing legs"})
    elif a1["clusters"] < rule["min_clusters"]:
        out.update({"outcome": "inconclusive", "reason": f"arm venue clusters {a1['clusters']} < {rule['min_clusters']}"})
    elif (a1["mean_sharpe"] > c1["mean_sharpe"] and a1["mean_ddadj"] > c1["mean_ddadj"]
          and a2["mean_sharpe"] > c2["mean_sharpe"] and a2["mean_ddadj"] > c2["mean_ddadj"]):
        out.update({"outcome": "pass", "reason": "beats control on mean Sharpe and mean ddadj at cost x1 and x2"})
    elif a1["mean_sharpe"] < c1["mean_sharpe"] and a1["mean_ddadj"] < c1["mean_ddadj"]:
        out.update({"outcome": "fail", "reason": "loses to control on mean Sharpe and mean ddadj at cost x1"})
    else:
        out.update({"outcome": "inconclusive", "reason": "mixed result against control"})
    return out


def suggest_cmd() -> None:
    from regime_stats import benjamini_hochberg
    proto = protocol()
    require_lock()
    sel = require_selection()
    noise = load_json(NOISE_PATH)
    res = load_json(RESULTS_PATH)
    fid = load_json(FIDELITY_PATH)
    cov = load_json(COVERAGE_PATH)
    with gzip.open(SAMPLES_PATH, "rt") as fh:
        samples = json.load(fh)["samples"]
    if res["selection_sha256"] != sha256_file(SELECTION_PATH) or fid["selection_sha256"] != sha256_file(SELECTION_PATH):
        raise SystemExit("results.json or fidelity.json was written under a different selection.json")
    st = proto["statistics"]
    vr = proto["verdict_rule"]
    pooled_w = proto["proxy"]["pooled_windows"]
    frames_out, tests = {}, []
    for frame, fr in sorted(proto["frames"].items()):
        excluded = frame_exclusions(proto, frame, res, fid, cov)
        all_ds = [tuple(d) for d in fr["datasets"]]
        keep = {d for d in frame_all_datasets(proto, frame) if d not in excluded}
        kept_main = [d for d in all_ds if d in keep]
        arms = {}
        for arm in sel["frames"][frame]["judged_arms"]:
            spec = fr["arms"][arm]
            base_rows = pooled_rows(samples, frame, arm, "net", pooled_w, keep)
            stress_rows = pooled_rows(samples, frame, arm, "stress", pooled_w, keep)
            gross_rows = pooled_rows(samples, frame, arm, "gross", pooled_w, keep)
            stats = cluster_stats(base_rows, st["n_resamples"], st["seed"]) if base_rows else None
            p_plain = stats["permutation"]["p_value"] if stats else 1.0
            p_cluster = stats["cluster_permutation"]["p_value"] if stats else 1.0
            nb = spec["noise_baseline"]
            nv = noise["baselines"][f"{frame}:{nb}"]["verdict"]
            item = {
                "family": spec["family"], "control": spec["control"], "noise_baseline": nb, "noise_verdict": nv,
                "trades": len(base_rows),
                "clusters": len(set(cluster_ids([(r["entry"], r["exit"]) for r in base_rows]).values())),
                "net_per_trade_pct": round(statistics.fmean(r["value"] for r in base_rows), 6) if base_rows else None,
                "stress_per_trade_pct": (round(statistics.fmean(r["value"] for r in stress_rows), 6)
                                         if stress_rows else None),
                "gross_per_trade_pct": (round(statistics.fmean(r["value"] for r in gross_rows), 6)
                                        if gross_rows else None),
                "p_plain": p_plain, "p_cluster": p_cluster, "p_decision": max(p_plain, p_cluster),
                "statistics": stats,
                "datasets": [f"{c} {t}" for c, t in arm_datasets(proto, frame, arm) if (c, t) in keep],
            }
            if fr.get("venue"):
                item["venue"] = venue_verdict(proto, res, frame, arm, keep)
            arms[arm] = item
            tests.append((frame, arm, item["p_decision"]))
        frames_out[frame] = {"excluded": {f"{c} {t}": v for (c, t), v in sorted(excluded.items())},
                             "kept_datasets": [f"{c} {t}" for c, t in kept_main],
                             "datasets_total": len(all_ds), "arms": arms}
    passed = benjamini_hochberg([p for _, _, p in tests], alpha=st["alpha"], family_size=st["family_size"]) \
        if tests else []
    for (frame, arm, _), ok in zip(tests, passed):
        frames_out[frame]["arms"][arm]["bh_pass"] = bool(ok)
    for frame, fo in sorted(frames_out.items()):
        fr = proto["frames"][frame]
        positive, floor_only = [], []
        half_ok = len(fo["kept_datasets"]) * 2 >= fo["datasets_total"]
        for arm, item in sorted(fo["arms"].items()):
            c = {
                "baseline_has_gross_edge": item["noise_verdict"] != "no_positive_edge",
                "net_positive_base": item["net_per_trade_pct"] is not None and item["net_per_trade_pct"] > 0,
                "net_positive_stress": item["stress_per_trade_pct"] is not None and item["stress_per_trade_pct"] > 0,
                "bh_pass": bool(item.get("bh_pass")),
                "min_trades": item["trades"] >= vr["min_trades"],
                "min_clusters": item["clusters"] >= vr["min_clusters"],
                "half_the_datasets_remain": half_ok,
            }
            if fr.get("venue"):
                c["venue_not_fail"] = item["venue"]["outcome"] != "fail"
            item["checks"] = c
            floor_keys = ("min_trades", "min_clusters", "half_the_datasets_remain")
            if all(c.values()):
                positive.append(arm)
            elif all(v for k, v in c.items() if k not in floor_keys):
                floor_only.append(arm)
        if positive:
            best = min(positive, key=lambda a: (fo["arms"][a]["p_decision"], -fo["arms"][a]["net_per_trade_pct"], a))
            fo.update({"verdict": "positive", "arm": best, "positive_arms": positive})
        elif not fo["arms"]:
            fo.update({"verdict": "negative", "arm": None,
                       "reason": "no filter arm met the frozen is shortlist rule, so no arm was judged"})
        elif floor_only:
            fo.update({"verdict": "inconclusive", "arm": None,
                       "reason": "judged arms passed every other check but missed a sample or dataset floor: "
                                 + ", ".join(floor_only)})
        else:
            fo.update({"verdict": "negative", "arm": None,
                       "reason": "no judged arm met every frozen verdict check"})
    verdict = {"protocol_sha256": protocol_sha256(), "selection_sha256": sha256_file(SELECTION_PATH),
               "noise_gate_sha256": sha256_file(NOISE_PATH), "results_sha256": sha256_file(RESULTS_PATH),
               "fidelity_sha256": sha256_file(FIDELITY_PATH), "family_size": st["family_size"],
               "tests": [{"frame": f, "arm": a, "p_decision": p} for f, a, p in tests], "frames": frames_out}
    perps = [f for f in ("paper", "live") if frames_out[f]["verdict"] == "positive"]
    suggested = []
    for path in [os.path.join(HERE, f"suggested_{f}_config.json") for f in ("paper", "live")]:
        if os.path.exists(path):
            os.remove(path)
    for frame in perps:
        arm = frames_out[frame]["arm"]
        fr = proto["frames"][frame]
        picks = sel["frames"][frame]["picks_before_combo"]
        rule = resolved_rule(proto, frame, arm, picks)
        if rule.get("interval"):
            frames_out[frame]["suggestion_note"] = "the winning arm moves the bar size; no copy config is written"
            continue
        shape = deployed_shape(fr["config_source"])
        filtered = apply_config_rule(shape, rule)
        keep_ids = set()
        for ds in frames_out[frame]["arms"][arm]["datasets"]:
            coin, tf = ds.split(" ")
            keep_ids.add(config_strategy_id(shape, coin.split("/")[0], tf))
        copies = []
        for sc in filtered["strategies"]:
            if sc["id"] in keep_ids:
                g = copy.deepcopy(sc)
                g["id"] = sc["id"].replace(f"-{fr['config_source']}", f"-filtered-{fr['config_source']}")
                copies.append(g)
        filtered["strategies"] = sorted(shape["strategies"] + copies, key=lambda sc: sc["id"])
        path = os.path.join(HERE, f"suggested_{frame}_config.json")
        write_json(path, filtered)
        suggested.append(os.path.basename(path))
        frames_out[frame]["suggested_config"] = os.path.basename(path)
        frames_out[frame]["filtered_ids"] = sorted(g["id"] for g in copies)
    verdict["suggested_configs"] = suggested
    spot_v = frames_out["spot"]["verdict"]
    perps_v = {f: frames_out[f]["verdict"] for f in ("paper", "live")}
    verdict["frames_agree"] = len({spot_v, *perps_v.values()}) == 1
    verdict["suggestion_basis"] = "the perps frames decide the suggestion; the spot frame is reported beside them"
    write_json(VERDICT_PATH, verdict)
    for f, fo in sorted(frames_out.items()):
        print(f"{f}: {fo['verdict']} {fo.get('arm')} {fo.get('reason', '')}")


def time_leg_cmd(cache_db: str, work_dir: str) -> None:
    proto = protocol()
    require_lock()
    cov = load_json(COVERAGE_PATH)
    frozen, cache_db = prepare_offline(cache_db, work_dir)
    worker_init(cache_db, work_dir)
    import eval_windows as ew
    import pandas as pd
    t = proto["timing"]
    frame, coin, tf = t["frame"], t["coin"], t["interval"]
    w = proto["proxy"]["selection_window"]
    window = study_windows(proto)[w]
    fr = proto["frames"][frame]
    rng = proxy_effective_range(proxy_symbol(coin), tf, window, fr["warmup_bars"])
    import regime
    real = regime.bounded_window_labels
    calls = []

    def timed(df, **kw):
        t1 = time.time()
        r = real(df, **kw)
        calls.append({"bars": len(df), "windows": sorted((kw.get("windows_spec") or {}).keys()),
                      "seconds": time.time() - t1})
        return r
    regime.bounded_window_labels = timed
    labels = DiskLabelCache(label_cache_root(work_dir, "labels_timing"))
    reg = _WORKER["regs"][fr["registry"]]
    legs = {}
    for name, arm, kind in (("cold_control", "control", "net"), ("cached_control", "control", "net"),
                            ("cached_gross", "control", "gross"), ("cached_stress", "control", "stress"),
                            ("cached_htf", "htf", "net")):
        cand = build_arm_candidate(proto, frame, arm, coin, tf)
        t1 = time.time()
        leg = ew.run_candidate_leg(reg, cand, proxy_symbol(coin), tf, window, proto["capital_usd"], keep_trades=True,
                                   funding_mode=proto["funding_mode"], warmup_bars=fr["warmup_bars"],
                                   regime_label_cache=labels, **leg_cost_kwargs(proto, kind))
        legs[name] = {"seconds": round(time.time() - t1, 1), "trades": leg["trades"]}
    regime.bounded_window_labels = real
    label_s = sum(c["seconds"] for c in calls)
    per_window_bar = label_s / max(1, sum(c["bars"] * len(c["windows"]) for c in calls))
    stage = {}

    def frame_bars(u):
        if u["group"] == "proxy":
            s, e = u["range"]
            per = {"30m": 48, "1h": 24, "4h": 6}[u["interval"]]
            return (pd.Timestamp(e) - pd.Timestamp(s)).days * per + proto["frames"][u["frame"]]["warmup_bars"]
        per = {"30m": 48, "1h": 24}[u["interval"]]
        return 400 + (96 if u["interval"] == "30m" else 191) * per

    def window_count(u):
        names = set()
        for arm, _ in u["legs"]:
            cand = build_arm_candidate(proto, u["frame"], arm, u["coin"], u["interval"])
            plan = cand.get("regime_label_windows")
            if plan:
                names |= {plan["windows"][f] for f in ("gate", "directional", "atr")} | {plan["primary"]}
        return len(names)

    every_arm = {f: selection_arms(proto, f) for f in proto["frames"]}
    sel_units = select_units(proto, cov, every_arm)
    ctrl = {f: list(proto["frames"][f]["noise_baselines"]) for f in proto["frames"]}
    nz_units = noise_units(proto, cov)
    jd_units = []
    for f in proto["frames"]:
        for w2 in proto["proxy"]["judged_windows"]:
            jd_units += proxy_units(proto, cov, f, w2, ctrl[f], ["net"])
        jd_units += manifest_units(proto, cov, f, ctrl[f])
    for name, units in (("select", sel_units), ("noise_gate", nz_units), ("judge_baselines", jd_units)):
        seen, bars = set(), 0
        for u in units:
            key = (u["frame"], u["group"], u["coin"], u["interval"], u["window"])
            if key in seen:
                continue
            seen.add(key)
            bars += frame_bars(u) * window_count(u)
        stage[name] = {"label_window_bars": bars,
                       "process_hours": round(bars * per_window_bar / 3600, 1)}
    fid_bars = 0
    for f, fr2 in proto["frames"].items():
        if f == "spot":
            continue
        for w2 in proto["fidelity"]["proxy_windows"]:
            s, e = study_windows(proto)[w2]
            for c2, t2 in included_proxy(cov, f, w2):
                fid_bars += (pd.Timestamp(e) - pd.Timestamp(s)).days * {"30m": 48, "1h": 24, "4h": 6}[t2]
    stage["fidelity_composite_rows"] = {"label_window_bars": fid_bars,
                                        "process_hours": round(fid_bars * per_window_bar / 3600, 1)}
    total = sum(v["process_hours"] for v in stage.values())
    out = {"frame": frame, "dataset": f"{coin} {tf}", "window": w, "effective_range": rng, "legs": legs,
           "label_calls": calls, "label_seconds": round(label_s, 1),
           "seconds_per_window_bar": round(per_window_bar, 5), "stages": stage,
           "projected_process_hours_total": round(total, 1),
           "note": "label window-bars count each distinct (frame, group, dataset, window) frame once per label "
                   "window that any of its arms reads; venue and bridge frames are approximated from the manifest "
                   "spans; the judge stage adds label windows only for shortlisted gate arms on a new window",
           "machine": {"cpus": os.cpu_count(), "load_avg": list(os.getloadavg())},
           "measured_at_utc": now_utc()}
    write_json(TIMING_PATH, out)
    print(json.dumps({k: v for k, v in out.items() if k != "label_calls"}, indent=2))


def cache_parity_cmd(cache_db: str, work_dir: str) -> None:
    proto = protocol()
    require_lock()
    frozen, cache_db = prepare_offline(cache_db, work_dir)
    worker_init(cache_db, work_dir)
    import eval_windows as ew
    cp = proto["cache_parity"]
    frame, coin, tf, w = cp["frame"], cp["coin"], cp["interval"], cp["window"]
    fr = proto["frames"][frame]
    window = study_windows(proto)[w]
    rows = {}
    for arm in cp["arms"]:
        cand = build_arm_candidate(proto, frame, arm, coin, tf)
        legs = {}
        for mode in ("uncached", "cached"):
            t0 = time.time()
            cache = DiskLabelCache(label_cache_root(work_dir, "labels_parity")) if mode == "cached" else None
            leg = ew.run_candidate_leg(_WORKER["regs"][fr["registry"]], copy.deepcopy(cand), proxy_symbol(coin), tf,
                                       window, proto["capital_usd"], keep_trades=True,
                                       funding_mode=proto["funding_mode"], warmup_bars=fr["warmup_bars"],
                                       regime_label_cache=cache)
            legs[mode] = {"json": json.dumps(leg, sort_keys=True, default=str), "seconds": round(time.time() - t0, 1)}
        same = legs["uncached"]["json"] == legs["cached"]["json"]
        rows[arm] = {"identical": same, "sha256_uncached": sha256_bytes(legs["uncached"]["json"].encode()),
                     "sha256_cached": sha256_bytes(legs["cached"]["json"].encode()),
                     "seconds": {m: v["seconds"] for m, v in legs.items()},
                     "trades": json.loads(legs["cached"]["json"])["trades"]}
        print(f"{arm}: identical {same} {rows[arm]['seconds']}", flush=True)
    out = {"frame": frame, "dataset": f"{proxy_symbol(coin)} {tf}", "window": w, "range": list(window),
           "warmup_bars": fr["warmup_bars"], "rows": rows, "identical": all(r["identical"] for r in rows.values()),
           "engine_sha256": engine_sha256(), "source_commit": git_head(), "network_calls": len(NETWORK_CALLS)}
    write_json(CACHE_PARITY_PATH, out)
    if not out["identical"]:
        raise SystemExit("cached and uncached legs differ")
    print(f"wrote {CACHE_PARITY_PATH}")


def htf_parity_signals(cache_db: str) -> dict:
    import numpy as np
    import pandas as pd
    import storage
    from htf_filter import apply_htf_filter
    from htf_trend import derive_htf_candles, frozen_htf_trend
    from registry_loader import load_registry
    proto = protocol()
    hp = proto["htf_parity"]
    frame, coin, tf, w = hp["frame"], hp["coin"], hp["interval"], hp["window"]
    fr = proto["frames"][frame]
    start, end = study_windows(proto)[w]
    full = candle_frame(candle_file(coin, tf))
    step = pd.Timedelta(milliseconds=TF_MS[tf])
    s, e = pd.Timestamp(start), pd.Timestamp(end)
    leg_frame = full[(full.index >= s - step * fr["warmup_bars"]) & (full.index < e)].copy()
    htf_derived, _ = derive_htf_candles(leg_frame, tf)
    first_bucket = htf_derived.index[0]
    htf_name = candle_file(coin, hp["exchange_htf"])
    exch = candle_frame(htf_name)
    exch = exch[(exch.index >= first_bucket) & (exch.index < pd.Timestamp(proto["proxy"]["pinned_end"]))]
    set_cache(cache_db)
    storage.init_db(cache_db)
    ltf_rows = full.reset_index(drop=True)
    storage.store_ohlcv(ltf_rows[["timestamp", "open", "high", "low", "close", "volume"]], "binanceus",
                        proxy_symbol(coin), tf, db_path=cache_db)
    storage.store_ohlcv(exch.reset_index(drop=True)[["timestamp", "open", "high", "low", "close", "volume"]],
                        "binanceus", proxy_symbol(coin), hp["exchange_htf"], db_path=cache_db)
    block_network()
    import run_backtest
    existing = run_backtest._htf_trend_series(proxy_symbol(coin), tf, leg_frame.index)
    derived, dcov, _ = frozen_htf_trend(leg_frame, tf)
    reg = load_registry(fr["registry"])
    sig = reg.apply_strategy("tema_cross", leg_frame, {})["signal"].fillna(0).astype(int).to_numpy()
    f_exist = np.array([apply_htf_filter(int(a), int(b)) for a, b in zip(sig, existing.to_numpy())])
    f_deriv = np.array([apply_htf_filter(int(a), int(b)) for a, b in zip(sig, derived.to_numpy())])
    trend_eq = existing.to_numpy() == derived.to_numpy()
    sig_eq = f_exist == f_deriv
    exch_close = exch["close"].reindex(htf_derived.index)
    close_eq = bool(np.allclose(exch_close.to_numpy(), htf_derived["close"].to_numpy(), rtol=0, atol=0))
    return {"dataset": f"{proxy_symbol(coin)} {tf}", "window": w, "warmup_bars": fr["warmup_bars"],
            "frame_first_bar": str(leg_frame.index[0]), "frame_last_bar": str(leg_frame.index[-1]),
            "frame_bars": int(len(leg_frame)), "exchange_htf_file": htf_name,
            "exchange_htf_rows_cached": int(len(exch)), "cache_cut_at_first_bucket": str(first_bucket),
            "derived_coverage": dcov, "bucket_closes_identical": close_eq,
            "trend_identical": bool(trend_eq.all()), "trend_mismatch_bars": int((~trend_eq).sum()),
            "filtered_signal_identical": bool(sig_eq.all()), "filtered_signal_mismatch_bars": int((~sig_eq).sum()),
            "signals_raw": {"long": int((sig == 1).sum()), "short": int((sig == -1).sum())},
            "signals_filtered": {"long": int((f_deriv == 1).sum()), "short": int((f_deriv == -1).sum())},
            "fetch_calls": len(NETWORK_CALLS)}


def htf_parity_leg(cache_db: str, work_dir: str) -> dict:
    import pandas as pd
    import htf_trend
    proto = protocol()
    require_lock()
    frozen, cache_db = prepare_offline(cache_db, work_dir)
    worker_init(cache_db, work_dir)
    import eval_windows as ew
    import storage
    hp = proto["htf_parity"]
    frame, coin, tf, w = hp["frame"], hp["coin"], hp["interval"], hp["window"]
    fr = proto["frames"][frame]
    window = study_windows(proto)[w]
    cand = build_arm_candidate(proto, frame, "htf", coin, tf)
    labels = DiskLabelCache(label_cache_root(work_dir, "labels_htf_parity"))

    def run():
        return ew.run_candidate_leg(_WORKER["regs"][fr["registry"]], copy.deepcopy(cand), proxy_symbol(coin), tf,
                                    window, proto["capital_usd"], keep_trades=True,
                                    funding_mode=proto["funding_mode"], warmup_bars=fr["warmup_bars"],
                                    regime_label_cache=labels)
    derived_leg = run()
    real = htf_trend.frozen_htf_trend
    exch = candle_frame(candle_file(coin, hp["exchange_htf"]))
    import run_backtest
    injected = {}

    def from_existing(ltf_df, ltf_timeframe, ema_period=50):
        trend, cov, htf_df = real(ltf_df, ltf_timeframe, ema_period)
        first = htf_df.index[0]
        rows = exch[(exch.index >= first) & (exch.index < pd.Timestamp(proto["proxy"]["pinned_end"]))]
        storage.store_ohlcv(rows.reset_index(drop=True)[["timestamp", "open", "high", "low", "close", "volume"]],
                            "binanceus", proxy_symbol(coin), hp["exchange_htf"])
        ex_trend = run_backtest._htf_trend_series(proxy_symbol(coin), ltf_timeframe, ltf_df.index)
        injected["equal"] = bool((ex_trend.to_numpy() == trend.to_numpy()).all())
        return ex_trend, cov, htf_df
    htf_trend.frozen_htf_trend = from_existing
    try:
        existing_leg = run()
    finally:
        htf_trend.frozen_htf_trend = real
    a = json.dumps(derived_leg, sort_keys=True, default=str)
    b = json.dumps(existing_leg, sort_keys=True, default=str)
    return {"dataset": f"{proxy_symbol(coin)} {tf}", "window": w, "arm": "htf", "identical": a == b,
            "sha256_derived": sha256_bytes(a.encode()), "sha256_existing_path": sha256_bytes(b.encode()),
            "trades": derived_leg["trades"], "existing_trend_equal_on_leg_frame": injected.get("equal"),
            "htf_block": derived_leg.get("htf_filter"), "fetch_calls": len(NETWORK_CALLS)}


def htf_parity_cmd(mode: str, cache_db: str, work_dir: str) -> None:
    held = load_json(HTF_PARITY_PATH) if os.path.exists(HTF_PARITY_PATH) else {}
    if mode == "signals":
        if os.path.exists(LOCK_PATH):
            raise SystemExit("the signal parity runs before the lock")
        cache_db = refuse_default_db(cache_db)
        verify_frozen()
        held["signals"] = htf_parity_signals(cache_db)
        held["signals"]["measured_at_utc"] = now_utc()
        write_json(HTF_PARITY_PATH, held)
        s = held["signals"]
        print(f"signals: trend identical {s['trend_identical']} filtered identical {s['filtered_signal_identical']} "
              f"fetch calls {s['fetch_calls']}")
        if not (s["trend_identical"] and s["filtered_signal_identical"] and s["fetch_calls"] == 0):
            raise SystemExit("frozen higher-timeframe parity failed; stop and report")
    else:
        held["leg"] = htf_parity_leg(cache_db, work_dir)
        held["leg"]["measured_at_utc"] = now_utc()
        write_json(HTF_PARITY_PATH, held)
        print(f"leg: identical {held['leg']['identical']} trades {held['leg']['trades']}")
        if not held["leg"]["identical"]:
            raise SystemExit("the leg scored with the existing-path trend differs; stop and report")


def synthetic_frame(tf: str, n: int, seed: int, end: str):
    import numpy as np
    import pandas as pd
    step = TF_MS[tf]
    end_ms = to_ms(end)
    ts = end_ms - step * np.arange(n, 0, -1)
    rng = np.random.default_rng(seed)
    k = np.arange(n)
    drift = np.where(k < n * 0.7, 0.0006, -0.0009) * (TF_MS[tf] / 1_800_000) ** 0.5
    wave = 0.012 * np.sin(k / 37.0) - 0.012 * np.sin((k - 1) / 37.0)
    lr = rng.normal(0, 0.004, n) + drift + wave
    close = 100 * np.exp(np.cumsum(lr))
    op = np.r_[close[0], close[:-1]]
    hi = np.maximum(op, close) * (1 + np.abs(rng.normal(0, 0.002, n)))
    lo = np.minimum(op, close) * (1 - np.abs(rng.normal(0, 0.002, n)))
    return pd.DataFrame({"timestamp": ts.astype("int64"), "open": op, "high": hi, "low": lo, "close": close,
                         "volume": 1.0})


def smoke_frame(task):
    frame, work_dir = task
    import pandas as pd
    proto = protocol()
    fr = proto["frames"][frame]
    path = os.path.join(work_dir, f"smoke_{frame}.db")
    if os.path.exists(path):
        os.remove(path)
    set_cache(path)
    for p in (os.path.join(REPO, "shared_tools"), BACKTEST):
        if p not in sys.path:
            sys.path.insert(0, p)
    import storage
    storage.init_db(path)
    sm = proto["smoke"]
    scored = sm["scored_bars"]
    datasets = sorted({d for arm in selection_arms(proto, frame) for d in arm_datasets(proto, frame, arm)})
    for i, (coin, tf) in enumerate(datasets):
        df = synthetic_frame(tf, fr["warmup_bars"] + scored[tf], 100 + i, sm["end"])
        storage.store_ohlcv(df, "binanceus", proxy_symbol(coin), tf, db_path=path)
    block_network()
    from registry_loader import load_registry
    import eval_windows as ew
    import backtester
    reg = load_registry(fr["registry"])
    labels = DiskLabelCache(os.path.join(work_dir, "smoke_labels", frame))
    captured = {}
    real_run = backtester.Backtester.run

    def capture(self, df, *a, **k):
        r = real_run(self, df, *a, **k)
        captured["trades"] = r.get("trades") or []
        captured["frame"] = k.get("indicator_frame") if k.get("indicator_frame") is not None else df
        captured["atr_method"] = self.atr_method
        return r
    backtester.Backtester.run = capture
    rows = {}
    try:
        for arm in selection_arms(proto, frame):
            spec = fr["arms"][arm]
            if spec.get("alias"):
                rows[arm] = {"alias_of": spec["alias"]}
                continue
            for coin, tf in arm_datasets(proto, frame, arm):
                cand = build_arm_candidate(proto, frame, arm, coin, tf)
                end = pd.Timestamp(sm["end"])
                start = end - pd.Timedelta(milliseconds=TF_MS[tf] * scored[tf])
                t0 = time.time()
                leg = ew.run_candidate_leg(reg, cand, proxy_symbol(coin), tf, (str(start), str(end)),
                                           proto["capital_usd"], keep_trades=True, funding_mode="off",
                                           warmup_bars=fr["warmup_bars"],
                                           regime_label_cache=labels if cand.get("regime_label_windows") else None)
                atr_rows = []
                from atr import standard_atr
                hist = captured["frame"]
                wild = standard_atr(hist, method="wilder")
                simp = standard_atr(hist, method="simple")
                for tr in captured["trades"][:5]:
                    ea = float(tr.get("entry_atr") or 0.0)
                    if ea <= 0:
                        continue
                    pos = hist.index.searchsorted(pd.Timestamp(tr["entry_date"]))
                    cands_w = [float(wild.iloc[j]) for j in (pos - 1, pos) if 0 <= j < len(wild)]
                    cands_s = [float(simp.iloc[j]) for j in (pos - 1, pos) if 0 <= j < len(simp)]
                    atr_rows.append({"entry": tr["entry_date"], "entry_atr": ea,
                                     "matches_wilder": any(abs(ea - x) <= 1e-6 * max(1.0, x) for x in cands_w),
                                     "matches_simple": any(abs(ea - x) <= 1e-6 * max(1.0, x) for x in cands_s)})
                ex = leg.get("execution") or {}
                rows[f"{arm} {coin} {tf}"] = {
                    "ran": True, "trades": leg["trades"], "long_positions": ex.get("long_positions"),
                    "short_positions": ex.get("short_positions"), "seconds": round(time.time() - t0, 1),
                    "engine_atr_method": captured["atr_method"], "entry_atr_checks": atr_rows,
                    "htf_filter": leg.get("htf_filter"),
                    "perps_sizing": (cand.get("stop_context") or {}).get("perps_sizing"),
                    "liquidated": leg.get("liquidated")}
    finally:
        backtester.Backtester.run = real_run
    os.remove(path)
    return frame, rows, len(NETWORK_CALLS)


def smoke_cmd(work_dir: str) -> None:
    proto = protocol()
    if os.path.exists(LOCK_PATH):
        raise SystemExit("smoke runs before the lock")
    os.makedirs(work_dir, exist_ok=True)
    ctx = multiprocessing.get_context("spawn")
    out = {"frames": {}, "network_calls": 0, "note": proto["smoke"]["note"]}
    with ctx.Pool(processes=len(proto["frames"])) as pool:
        for frame, rows, calls in pool.imap_unordered(smoke_frame, [(f, work_dir) for f in sorted(proto["frames"])]):
            out["frames"][frame] = rows
            out["network_calls"] += calls
            print(f"[smoke] {frame}: {len(rows)} rows", flush=True)
    checks = {}
    live = out["frames"]["live"]
    checks["live_both_opened_a_short"] = any(v.get("short_positions") for k, v in live.items()
                                             if k.startswith("live_both "))
    atr_rows = [r for fr in ("paper", "live") for v in out["frames"][fr].values() for r in v.get("entry_atr_checks") or []]
    checks["entry_atr_rows"] = len(atr_rows)
    checks["entry_atr_is_wilder"] = bool(atr_rows) and all(r["matches_wilder"] and not r["matches_simple"]
                                                           for r in atr_rows)
    checks["every_arm_ran"] = all(v.get("ran") or v.get("alias_of") for fr in out["frames"].values()
                                  for v in fr.values())
    checks["network_calls"] = out["network_calls"]
    out["checks"] = checks
    out["live_sizing"] = next((v["perps_sizing"] for k, v in live.items() if k.startswith("live_both ")), None)
    out["measured_at_utc"] = now_utc()
    write_json(SMOKE_PATH, out)
    print(json.dumps(checks, indent=2))
    print(f"live sizing: {out['live_sizing']}")
    if not (checks["every_arm_ran"] and checks["live_both_opened_a_short"] and checks["entry_atr_is_wilder"]
            and checks["network_calls"] == 0):
        raise SystemExit("smoke check failed; stop and report")


def main(argv=None) -> int:
    p = argparse.ArgumentParser(description="#2016 tema_cross entry-filter study (suggest-only)")
    sub = p.add_subparsers(dest="cmd", required=True)
    sub.add_parser("sanitize-check")
    b = sub.add_parser("build-arms")
    b.add_argument("--check", action="store_true")
    sub.add_parser("copy-frozen")
    a = sub.add_parser("acquire-4h")
    a.add_argument("--cache-db", required=True)
    sub.add_parser("verify-frozen")
    s = sub.add_parser("smoke")
    s.add_argument("--work-dir", required=True)
    sub.add_parser("coverage")
    h = sub.add_parser("htf-parity")
    h.add_argument("--signals", action="store_true")
    h.add_argument("--leg", action="store_true")
    h.add_argument("--cache-db", required=True)
    h.add_argument("--work-dir")
    sub.add_parser("lock")
    sub.add_parser("hash")
    for name in ("cache-parity", "time-leg", "select", "noise-gate", "judge"):
        q = sub.add_parser(name)
        q.add_argument("--cache-db", required=True)
        q.add_argument("--work-dir", required=True)
        if name in ("select", "noise-gate", "judge"):
            q.add_argument("--workers", type=int, default=os.cpu_count() or 4)
    f = sub.add_parser("fidelity")
    f.add_argument("--work-dir", required=True)
    f.add_argument("--workers", type=int, default=os.cpu_count() or 4)
    sub.add_parser("suggest")
    sub.add_parser("render")
    args = p.parse_args(argv)
    cache = getattr(args, "cache_db", None)
    if cache:
        os.environ[CACHE_ENV] = os.path.abspath(cache)
    else:
        os.environ[CACHE_ENV] = os.path.join(tempfile.mkdtemp(prefix="tema_cross_2016_"), "unused.db")
    if args.cmd == "sanitize-check":
        sanitize_check()
    elif args.cmd == "build-arms":
        build_arms(args.check)
    elif args.cmd == "copy-frozen":
        copy_frozen()
    elif args.cmd == "acquire-4h":
        acquire_spot_4h(args.cache_db)
    elif args.cmd == "verify-frozen":
        frozen = verify_frozen()
        print(f"verified {len(frozen['proxy']['files'])} proxy files, "
              f"{len(frozen['proxy']['copied_files'])} copied venue, bridge and manifest files and "
              f"{len(frozen['manifests'])} manifests")
    elif args.cmd == "smoke":
        smoke_cmd(args.work_dir)
    elif args.cmd == "coverage":
        coverage()
    elif args.cmd == "htf-parity":
        if args.signals == args.leg:
            raise SystemExit("pass exactly one of --signals or --leg")
        if args.leg and not args.work_dir:
            raise SystemExit("--leg needs --work-dir")
        htf_parity_cmd("signals" if args.signals else "leg", args.cache_db, args.work_dir)
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
    elif args.cmd == "noise-gate":
        noise_gate_cmd(args.cache_db, args.work_dir, args.workers)
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

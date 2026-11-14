#!/usr/bin/env python3

import argparse
import base64
import copy
import gzip
import hashlib
import json
import math
import multiprocessing
import os
import shutil
import socket
import subprocess
import sys
import tempfile
import time
import zlib
from collections import OrderedDict
from collections.abc import MutableMapping

HERE = os.path.dirname(os.path.abspath(__file__))
BACKTEST = os.path.abspath(os.path.join(HERE, "..", ".."))
REPO = os.path.abspath(os.path.join(BACKTEST, ".."))
DATA = os.path.join(HERE, "data")
ARMS_DIR = os.path.join(HERE, "arms")
PROTOCOL_PATH = os.path.join(HERE, "protocol.json")
LOCK_PATH = os.path.join(HERE, "protocol_lock.json")
INPUTS_PATH = os.path.join(HERE, "study_manifest.json")
COVERAGE_PATH = os.path.join(HERE, "coverage_report.json")
ARM_VALIDATION_PATH = os.path.join(HERE, "arm_validation.json")
CHECKS_PATH = os.path.join(HERE, "checks.json")
SCORING_LOG_PATH = os.path.join(HERE, "scoring_log.json")
SELECTION_PATH = os.path.join(HERE, "selection.json")
TRAIN_RESULTS_PATH = os.path.join(HERE, "results_train.json")
TRAIN_SAMPLES_PATH = os.path.join(HERE, "samples_train.json.gz")
JUDGE_RESULTS_PATH = os.path.join(HERE, "results_judge.json")
JUDGE_SAMPLES_PATH = os.path.join(HERE, "samples_judge.json.gz")
CONTINUOUS_PATH = os.path.join(HERE, "continuous.json")
EQUITY_PATH = os.path.join(HERE, "continuous_equity.json.gz")
VERDICT_PATH = os.path.join(HERE, "verdict.json")
REPORT_PATH = os.path.join(HERE, "REPORT.md")
POST_CHECKS_PATH = os.path.join(HERE, "checks_post.json")
SOURCE_STUDY = os.path.join(BACKTEST, "candidates", "tema_cross_2016")
SHARED_CACHE = os.path.join(REPO, "shared_tools", "trading_bot.db")
CACHE_ENV = "GO_TRADER_OHLCV_CACHE_DB"
HOUR_MS = 3_600_000
DAY_MS = 86_400_000
LEAD_BARS = 250
HORIZON_BARS = 2000
LABELS = ("ranging_directional", "ranging_directional_down", "ranging_directional_up", "ranging_quiet",
          "ranging_volatile", "trending_down_choppy", "trending_down_clean", "trending_up_choppy",
          "trending_up_clean")
TARGET = "ranging_volatile"
CODE_FILES = (
    "backtest/backtester.py",
    "backtest/eval_windows.py",
    "backtest/exit_diagnostics.py",
    "backtest/exit_policy_ab.py",
    "backtest/offline_manifest.py",
    "backtest/regime_label_columns.py",
    "backtest/run_backtest.py",
    "backtest/tune_live.py",
    "backtest/candidates/mean_reversion_exit_2018/run_study.py",
    "shared_strategies/close/_helpers.py",
    "shared_strategies/close/post_tp_sl.py",
    "shared_strategies/close/regime_atr.py",
    "shared_strategies/close/registry.py",
    "shared_strategies/close/tiered_tp_atr.py",
    "shared_strategies/close/tiered_tp_atr_regime.py",
    "shared_strategies/close/trailing_tp_ratchet.py",
    "shared_strategies/open/adx_trend.py",
    "shared_strategies/open/indicators_core.py",
    "shared_strategies/open/registry.py",
    "shared_tools/atr.py",
    "shared_tools/closed_bar.py",
    "shared_tools/funding_fetcher.py",
    "shared_tools/regime.py",
)
GUARDED_FILES = (
    "backtest/research/regime_1152_exit_retune.json",
    "backtest/research/regime_1152_config_b2_mr.json",
    "backtest/research/regime_1152_config_ratchet_mr.json",
    "docs/research/1152-ranging-exit-geometry-m6.md",
)
SCORING_OUTPUTS = (SELECTION_PATH, TRAIN_RESULTS_PATH, TRAIN_SAMPLES_PATH, JUDGE_RESULTS_PATH, JUDGE_SAMPLES_PATH,
                   CONTINUOUS_PATH, EQUITY_PATH, VERDICT_PATH)
for _p in (os.path.join(REPO, "shared_tools"), BACKTEST, os.path.join(REPO, "shared_strategies", "close")):
    if _p not in sys.path:
        sys.path.insert(0, _p)


def sha256_bytes(data: bytes) -> str:
    return hashlib.sha256(data).hexdigest()


def sha256_file(path: str) -> str:
    h = hashlib.sha256()
    with open(path, "rb") as fh:
        for chunk in iter(lambda: fh.read(1 << 20), b""):
            h.update(chunk)
    return h.hexdigest()


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


def rel(path: str) -> str:
    return os.path.relpath(path, REPO)


def now_utc() -> str:
    return time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime())


def git(*args) -> str:
    return subprocess.check_output(["git", "-C", REPO, *args], text=True).strip()


def git_head() -> str:
    try:
        return git("rev-parse", "HEAD")
    except (OSError, subprocess.CalledProcessError):
        return "unknown"


def to_ms(day: str) -> int:
    import pandas as pd
    return int(pd.Timestamp(day, tz="UTC").timestamp() * 1000)


def protocol() -> dict:
    return load_json(PROTOCOL_PATH)


def code_sha256() -> dict:
    return {p: sha256_file(os.path.join(REPO, p)) for p in CODE_FILES}


def guarded_sha256() -> dict:
    out = {p: sha256_file(os.path.join(REPO, p)) for p in GUARDED_FILES}
    runs = os.path.join(BACKTEST, "research", "regime_1152_runs")
    for name in sorted(os.listdir(runs)):
        out[rel(os.path.join(runs, name))] = sha256_file(os.path.join(runs, name))
    out["shared_tools/trading_bot.db"] = sha256_file(SHARED_CACHE) if os.path.exists(SHARED_CACHE) else None
    return out


NETWORK_CALLS = []


def refuse_network(*_a, **_k):
    NETWORK_CALLS.append(1)
    raise RuntimeError("mean_reversion_exit_2018 scoring: network access is blocked")


def block_network() -> None:
    socket.socket.connect = refuse_network
    socket.socket.connect_ex = refuse_network
    socket.create_connection = refuse_network
    socket.getaddrinfo = refuse_network
    import data_fetcher
    import funding_fetcher
    import offline_manifest
    data_fetcher.fetch_full_history = refuse_network
    data_fetcher.fetch_ohlcv = refuse_network
    data_fetcher.get_exchange = refuse_network
    data_fetcher.load_cached_data = refuse_network
    funding_fetcher._hl_adapter = refuse_network
    offline_manifest._post_info = refuse_network
    import run_backtest
    run_backtest.get_exchange = refuse_network


def network_probe() -> dict:
    out = {}
    try:
        socket.create_connection(("api.hyperliquid.xyz", 443), timeout=5)
        out["socket_connect"] = "connected (unexpected)"
    except RuntimeError as exc:
        out["socket_connect"] = f"refused: {exc}"
    import data_fetcher
    try:
        data_fetcher.load_cached_data("BTC/USDT", "1h", start_date="2024-01-01", end_date="2024-01-02")
        out["cache_load"] = "loaded (unexpected)"
    except RuntimeError as exc:
        out["cache_load"] = f"refused: {exc}"
    import offline_manifest
    try:
        offline_manifest._post_info({"type": "meta"})
        out["venue_fetch"] = "fetched (unexpected)"
    except RuntimeError as exc:
        out["venue_fetch"] = f"refused: {exc}"
    if any("unexpected" in v for v in out.values()):
        raise SystemExit(f"network probe failed: {out}")
    return out


def isolate_process(work_dir: str) -> None:
    os.environ[CACHE_ENV] = os.path.join(work_dir, "never_used_cache.db")
    block_network()


def copy_inputs() -> None:
    proto = protocol()
    if os.path.exists(INPUTS_PATH):
        raise SystemExit("study_manifest.json exists; inputs are copied once")
    src_manifest = load_json(os.path.join(SOURCE_STUDY, "study_manifest.json"))
    src_files = {f"data/{k}": v for k, v in src_manifest["files"].items()}
    src_copied = src_manifest["copied_files"]
    files = {}
    for name in proto["inputs"]["copy"]:
        src = os.path.join(SOURCE_STUDY, name)
        expected = (src_files.get(name) or {}).get("sha256") or (src_copied.get(name) or {}).get("sha256")
        if expected is None:
            raise SystemExit(f"{name} is not recorded in the source study manifest")
        dst = os.path.join(HERE, name)
        os.makedirs(os.path.dirname(dst), exist_ok=True)
        shutil.copyfile(src, dst)
        got = sha256_file(dst)
        if got != expected:
            raise SystemExit(f"copied {name} sha256 {got} != source manifest {expected}")
        meta = {"source": rel(src), "sha256": got}
        src_meta = src_files.get(name)
        if src_meta:
            meta["source_metadata"] = {k: v for k, v in sorted(src_meta.items()) if k != "copied_from"}
        files[name] = meta
    write_json(INPUTS_PATH, {
        "study": proto["study"], "copied_at_utc": now_utc(), "source_commit": git_head(),
        "source_study_manifest": rel(os.path.join(SOURCE_STUDY, "study_manifest.json")),
        "source_study_manifest_sha256": sha256_file(os.path.join(SOURCE_STUDY, "study_manifest.json")),
        "pinned_end": proto["inputs"]["pinned_end"], "note": proto["inputs"]["note"], "files": files,
    })
    print(f"copied {len(files)} files byte for byte")


def proxy_manifest_path(tf: str) -> str:
    return os.path.join(HERE, f"proxy_manifest_{tf}.json")


def build_proxy_manifest(proto: dict, tf: str) -> dict:
    pm = proto["proxy_manifests"]
    venue = load_json(os.path.join(HERE, "venue_manifest_1h.json"))
    bridge_tf = "1h" if tf == "4h" else tf
    bridge = load_json(os.path.join(SOURCE_STUDY, f"bridge_manifest_{bridge_tf}.json"))
    spreads = {d["coin"]: d["half_spread_bps"] for d in bridge["datasets"]}
    sizes = {d["coin"]: d["size_decimals"] for d in bridge["datasets"]}
    inputs = load_json(INPUTS_PATH)["files"]
    coins = pm["coins_4h"] if tf == "4h" else pm["coins"]
    datasets = []
    for coin in coins:
        candles = f"data/proxy/{coin}_{tf}_candles.csv.gz"
        funding = f"data/proxy/{coin}_funding.csv.gz"
        datasets.append({"coin": coin, "symbol": f"{coin}/USDT", "size_decimals": sizes[coin],
                         "half_spread_bps": spreads[coin],
                         "candles": {"path": candles, "sha256": inputs[candles]["sha256"]},
                         "funding": {"path": funding, "sha256": inputs[funding]["sha256"]}})
    windows = {name: {"start": w["start"], "end": w["end"], "role": w["role"]}
               for name, w in sorted(proto["windows"].items())}
    return {
        "schema": "offline_candle_manifest/v1",
        "study": proto["study"],
        "venue": "hyperliquid",
        "provenance": {"kind": "proxy", "label": pm["provenance_label"]},
        "interval": tf,
        "warmup_bars": pm["warmup_bars"],
        "windows": windows,
        "acquisition": {"source": "no acquisition: byte copies of the tema_cross_2016 frozen proxy candles and "
                                  "Hyperliquid funding", "start": "2022-08-01", "end": proto["inputs"]["pinned_end"],
                        "note": pm["half_spread_source"]},
        "costs": venue["costs"],
        "venue_reference": {
            "meta": {"path": "data/venue_1h/venue_meta.json",
                     "sha256": inputs["data/venue_1h/venue_meta.json"]["sha256"]},
            "l2_snapshot": {"path": "data/venue_1h/venue_l2_snapshot.json",
                            "sha256": inputs["data/venue_1h/venue_l2_snapshot.json"]["sha256"]},
            "documents": venue["venue_reference"]["documents"],
        },
        "datasets": datasets,
    }


def build_manifests(check: bool) -> None:
    proto = protocol()
    for tf in proto["proxy_manifests"]["intervals"]:
        want = dumps(build_proxy_manifest(proto, tf))
        path = proxy_manifest_path(tf)
        if check:
            if not os.path.exists(path) or open(path).read() != want:
                raise SystemExit(f"{os.path.basename(path)} differs from a fresh build")
        else:
            with open(path, "w") as fh:
                fh.write(want)
    print("manifests ok" if check else "manifests written")


def verify_inputs() -> dict:
    import offline_manifest as om
    manifest = load_json(INPUTS_PATH)
    for name, meta in sorted(manifest["files"].items()):
        path = os.path.join(HERE, name)
        if not os.path.exists(path):
            raise SystemExit(f"frozen input {name} is missing")
        got = sha256_file(path)
        if got != meta["sha256"]:
            raise SystemExit(f"frozen input {name} sha256 {got} != study manifest {meta['sha256']}")
        src = os.path.join(REPO, meta["source"])
        if os.path.exists(src) and sha256_file(src) != got:
            raise SystemExit(f"{name} differs from its source {meta['source']}")
    loaded = {}
    proto = protocol()
    for tf in proto["proxy_manifests"]["intervals"]:
        loaded[("proxy", tf)] = om.load_manifest(proxy_manifest_path(tf), verify=True)
    for tf, name in sorted(proto["native"]["manifests"].items()):
        loaded[("native", tf)] = om.load_manifest(os.path.join(HERE, name), verify=True)
    return {"inputs": manifest, "manifests": loaded}


def inputs_digest() -> dict:
    out = {"study_manifest.json": sha256_file(INPUTS_PATH)}
    proto = protocol()
    for tf in proto["proxy_manifests"]["intervals"]:
        out[os.path.basename(proxy_manifest_path(tf))] = sha256_file(proxy_manifest_path(tf))
    for name in sorted(proto["native"]["manifests"].values()):
        out[name] = sha256_file(os.path.join(HERE, name))
    return out


def tp(m: float, f: float) -> dict:
    return {"atr_multiple": m, "close_fraction": f}


def rung(m: float, f: float, t: float) -> dict:
    return {"atr_multiple": m, "close_fraction": f, "trailing_mult_after": t}


def b2_default_ladder(label: str) -> list:
    from regime_atr import REGIME_TP_TIER_GROUP_DEFAULTS, regime_close_default_group
    return [tp(m, f) for m, f in REGIME_TP_TIER_GROUP_DEFAULTS[regime_close_default_group(label)]]


def ratchet_default_ladder(label: str) -> list:
    from trailing_tp_ratchet import DEFAULT_RATCHET_TIERS_BY_GROUP, ratchet_close_default_group
    return [rung(t["atr_multiple"], t["close_fraction"], t["trailing_mult_after"])
            for t in DEFAULT_RATCHET_TIERS_BY_GROUP[ratchet_close_default_group(label)]]


def trailing_default(label: str) -> float:
    from regime_atr import REGIME_ATR_DEFAULTS_TRAILING
    return REGIME_ATR_DEFAULTS_TRAILING[label].atr


def strategy_entry(proto: dict, frame: str, arm: str, coin: str, tf: str) -> dict:
    return {
        "id": f"mr-exit-2018-{frame}-{arm.replace('_', '-')}-{coin.lower()}-{tf}",
        "type": "perps", "platform": "hyperliquid", "script": "shared_scripts/check_hyperliquid.py",
        "args": ["mean_reversion", coin, tf, "--mode=paper"],
        "capital": proto["capital_usd"], "initial_capital": proto["capital_usd"], "leverage": 1,
        "interval_seconds": 300, "circuit_breaker": False,
        "direction": proto["frames"][frame]["direction"],
        "open_strategy": {"name": "mean_reversion", "params": dict(proto["open_strategy"]["params"])},
    }


def frame_coins(proto: dict, frame: str) -> list:
    out = set()
    for pairs in proto["frames"][frame]["groups"].values():
        out |= {tuple(p) for p in pairs}
    return sorted(out)


def arm_body(proto: dict, frame: str, arm: str) -> dict:
    fr = proto["frames"][frame]
    defs = fr["arm_definitions"]
    gate = {"allowed_regimes": list(fr["paired"]["gate"])} if arm.startswith("paired_") else {}
    if frame == "b2":
        if arm in ("paired_incumbent", "cont_historical"):
            return {"close_strategy": copy.deepcopy(defs["incumbent_close"]), **defs["incumbent_stop"], **gate}
        if arm.startswith("paired_b2_"):
            ladder = [tp(m, f) for m, f in defs["candidate_ladders"][arm]]
            return {"close_strategy": {"name": "tiered_tp_atr", "params": {"tp_tiers": ladder}},
                    **defs["incumbent_stop"], **gate}
        stop = defs["incumbent_stop"]["stop_loss_atr_mult"]
        paired = next((p for p, c in fr["continuous"]["arms"].items() if c == arm), None)
        table = {}
        for label in LABELS:
            ladder = b2_default_ladder(label)
            if paired and label == TARGET:
                ladder = [tp(m, f) for m, f in defs["candidate_ladders"][paired]]
            table[label] = {"stop_loss_atr": stop, "tp_tiers": ladder}
        return {"close_strategy": {"name": "tiered_tp_atr_regime", "params": {"trend_regime": table}}}
    paired = arm if arm.startswith("paired_r_") else next(
        (p for p, c in fr["continuous"]["arms"].items() if c == arm), None)
    tiers = {}
    for label in LABELS:
        ladder = ratchet_default_ladder(label)
        if paired and label == TARGET:
            ladder = [rung(m, f, t) for m, f, t in defs["candidate_ladders"][paired]]
        tiers[label] = ladder
    trail = {label: {"atr_multiple": trailing_default(label)} for label in LABELS}
    return {"close_strategy": {"name": "trailing_tp_ratchet_regime", "params": {"tp_tiers": tiers}},
            "trailing_stop_atr_mult_regime": {"trend_regime": trail}, **gate}


def frame_arm_names(proto: dict, frame: str) -> list:
    fr = proto["frames"][frame]
    names = [fr["paired"]["incumbent"], fr["paired"]["arms"]["primary"], *fr["paired"]["arms"]["neighbours"],
             fr["continuous"]["incumbent"], *fr["continuous"]["arms"].values()]
    if fr["continuous"].get("equivalence"):
        names.append(fr["continuous"]["equivalence"]["historical"])
    return sorted(set(names))


def build_arm_config(proto: dict, frame: str, arm: str) -> dict:
    regime = {k: copy.deepcopy(proto["regime"][k]) for k in ("enabled", "period", "windows")}
    strategies = []
    for coin, tf in frame_coins(proto, frame):
        sc = strategy_entry(proto, frame, arm, coin, tf)
        sc.update(copy.deepcopy(arm_body(proto, frame, arm)))
        strategies.append(dict(sorted(sc.items())))
    return {"config_version": 20, "regime": regime, "strategies": strategies}


def arm_path(frame: str, arm: str) -> str:
    return os.path.join(ARMS_DIR, frame, f"{arm}.json")


def build_arms(check: bool) -> None:
    proto = protocol()
    for frame in sorted(proto["frames"]):
        for arm in frame_arm_names(proto, frame):
            want = dumps(build_arm_config(proto, frame, arm))
            path = arm_path(frame, arm)
            if check:
                if not os.path.exists(path) or open(path).read() != want:
                    raise SystemExit(f"{rel(path)} differs from a fresh build")
            else:
                os.makedirs(os.path.dirname(path), exist_ok=True)
                with open(path, "w") as fh:
                    fh.write(want)
    print("arms ok" if check else "arms written")


def arms_sha256(proto: dict) -> dict:
    return {f"{frame}/{arm}.json": sha256_file(arm_path(frame, arm))
            for frame in sorted(proto["frames"]) for arm in frame_arm_names(proto, frame)}


def strategy_id_for(cfg: dict, coin: str, tf: str) -> str:
    hits = [s["id"] for s in cfg["strategies"] if s["args"][1] == coin and s["args"][2] == tf]
    if len(hits) != 1:
        raise SystemExit(f"expected one strategy for {coin} {tf}, found {hits}")
    return hits[0]


def arm_candidate(frame: str, arm: str, coin: str, tf: str) -> dict:
    import eval_windows as ew
    from run_backtest import load_strategy_config
    from tune_live import build_candidate
    path = arm_path(frame, arm)
    cfg = load_json(path)
    sid = strategy_id_for(cfg, coin, tf)
    res = load_strategy_config(path, sid, inject_user_defaults=True)
    open_ref = res["open_strategy"]
    cand = build_candidate(open_ref["name"], open_ref.get("params") or {}, res)
    cand["atr_method"] = res["atr_method"]
    ew.validate_candidate(copy.deepcopy(cand))
    return cand


def stop_owner(cand: dict) -> str:
    from backtester import build_stop_capability_context
    from run_backtest import stop_kwargs_from_json
    kw = stop_kwargs_from_json(cand["stop_context"]) if cand.get("stop_context") else {}
    fields = {k: v for k, v in kw.items() if k not in ("stop_platform", "strategy_type", "capability_context",
                                                        "perps_sizing", "liquidation_model", "venue_margin")}
    ctx = build_stop_capability_context(platform=kw.get("stop_platform") or "hyperliquid",
                                        strategy_type=kw.get("strategy_type") or "perps",
                                        close_refs=cand.get("close_strategies"), fields=fields,
                                        regime_windows_spec=cand.get("regime_windows_spec"),
                                        capability_context=kw.get("capability_context"))
    owner = ctx.resolved_stop_owner
    return owner["name"] if owner else None


def resolve_ratchet_table(params: dict) -> dict:
    from trailing_tp_ratchet import resolve_tiers_for_regime
    out = {}
    for label in LABELS:
        tiers, errs = resolve_tiers_for_regime(params, label, regime_table=True)
        out[label] = {"tiers": [list(t) for t in tiers], "errors": errs}
    return out


def resolve_trailing_table(raw: dict) -> dict:
    from regime_atr import SURFACE_TRAILING, parse_regime_atr_block, resolve_regime_atr
    block, errs = parse_regime_atr_block(raw, "trailing_stop_atr_mult_regime", SURFACE_TRAILING, LABELS)
    return {"errors": errs, "values": {label: resolve_regime_atr(block, label) for label in LABELS}}


def resolve_b2_table(params: dict) -> dict:
    from regime_atr import unified_regime_scalar_params
    from tiered_tp_atr_regime import regime_ladder_for
    out = {}
    for label in LABELS:
        lad = regime_ladder_for(params, label, True)
        stop = unified_regime_scalar_params(params, label)[1] if "trend_regime" in params else None
        out[label] = {"tiers": [list(t) for t in lad.get("tiers", [])], "reason": lad.get("reason"), "stop": stop}
    return out


def table_diff(a: dict, b: dict, key: str) -> list:
    return sorted(label for label in LABELS if a[label][key] != b[label][key])


def cand_core(cand: dict) -> dict:
    return {k: v for k, v in cand.items() if k not in ("close_strategies",)}


def validate_arms(go_binary: str) -> None:
    proto = protocol()
    build_arms(check=True)
    out = {"protocol_sha256": sha256_file(PROTOCOL_PATH), "arms_sha256": arms_sha256(proto), "frames": {},
           "go_inspect": {}, "measured_at_utc": now_utc(), "source_commit": git_head()}
    failures = []
    for frame in sorted(proto["frames"]):
        fr = proto["frames"][frame]
        rows = {}
        for arm in frame_arm_names(proto, frame):
            cfg = load_json(arm_path(frame, arm))
            sc = cfg["strategies"][0]
            per = {}
            for coin, tf in frame_coins(proto, frame):
                cand = arm_candidate(frame, arm, coin, tf)
                per[f"{coin} {tf}"] = {"direction": cand["direction"], "allowed_regimes": cand.get("allowed_regimes"),
                                       "close_strategies": cand.get("close_strategies"),
                                       "stop_owner": stop_owner(cand), "atr_method": cand["atr_method"],
                                       "label_plan_windows": (cand.get("regime_label_windows") or {}).get("windows"),
                                       "label_limit": (cand.get("regime_label_windows") or {}).get("limit")}
            owners = sorted({str(v["stop_owner"]) for v in per.values()})
            row = {"datasets": len(per), "stop_owners": owners,
                   "direction": sorted({v["direction"] for v in per.values()}),
                   "allowed_regimes": sorted({json.dumps(v["allowed_regimes"]) for v in per.values()}),
                   "atr_method": sorted({v["atr_method"] for v in per.values()}),
                   "label_limit": sorted({v["label_limit"] for v in per.values()}),
                   "close": sc["close_strategy"]}
            params = sc["close_strategy"]["params"]
            if frame == "ratchet":
                row["ratchet_table"] = resolve_ratchet_table(params)
                row["trailing_table"] = resolve_trailing_table(sc["trailing_stop_atr_mult_regime"])
            elif sc["close_strategy"]["name"] == "tiered_tp_atr_regime":
                row["b2_table"] = resolve_b2_table(params)
            rows[arm] = row
        checks = {}
        if frame == "ratchet":
            ref = resolve_ratchet_table({"use_defaults": True})
            ref_trail = resolve_trailing_table({"use_defaults": True})
            bare_only = copy.deepcopy(load_json(arm_path(frame, "cont_incumbent"))["strategies"][0]["close_strategy"]
                                      ["params"])
            for sub in ("ranging_directional_up", "ranging_directional_down"):
                bare_only["tp_tiers"].pop(sub)
            fallback = resolve_ratchet_table(bare_only)
            checks["directional_fallback"] = {
                "bare_key_only_serves_sublabels": fallback == ref,
                "sublabels": {s: fallback[s]["tiers"] for s in ("ranging_directional_up", "ranging_directional_down")}}
            for arm, row in rows.items():
                diff = table_diff(row["ratchet_table"], ref, "tiers")
                want = [] if arm in ("paired_incumbent", "cont_incumbent") else [TARGET]
                paired = arm if arm.startswith("paired_r_") else next(
                    (p for p, c in fr["continuous"]["arms"].items() if c == arm), None)
                target_ok = True
                if paired:
                    target_ok = row["ratchet_table"][TARGET]["tiers"] == fr["arm_definitions"]["candidate_ladders"][paired]
                no_errors = all(not row["ratchet_table"][l]["errors"] and row["ratchet_table"][l]["tiers"]
                                for l in LABELS)
                trail_ok = row["trailing_table"]["values"] == ref_trail["values"] and not row["trailing_table"]["errors"]
                ok = diff == want and target_ok and no_errors and trail_ok and row["stop_owners"] == ["trailing_atr_regime"]
                checks[arm] = {"labels_changed_vs_defaults": diff, "expected_changed": want, "target_ladder_ok": target_ok,
                               "every_label_has_exits": no_errors, "trail_equals_defaults": trail_ok, "ok": ok}
                if not ok:
                    failures.append(f"ratchet {arm}")
        else:
            ref = resolve_b2_table({"use_defaults": True})
            for arm, row in rows.items():
                table = row.get("b2_table")
                if arm.startswith("paired_b2_"):
                    want_ladder = [list(x) for x in fr["arm_definitions"]["candidate_ladders"][arm]]
                    got = [[t["atr_multiple"], t["close_fraction"]] for t in row["close"]["params"]["tp_tiers"]]
                    ok = got == want_ladder and row["stop_owners"] == ["fixed_atr"]
                    checks[arm] = {"scalar_ladder": got, "stop_owner": row["stop_owners"], "ok": ok}
                elif arm in ("paired_incumbent", "cont_historical"):
                    ok = (table_diff(table, ref, "tiers") == [] and table[TARGET]["tiers"] == [[0.5, 0.5], [1.0, 1.0]]
                          and row["stop_owners"] == ["fixed_atr"])
                    checks[arm] = {"labels_changed_vs_defaults": table_diff(table, ref, "tiers"),
                                   "volatile": table[TARGET]["tiers"], "stop_owner": row["stop_owners"], "ok": ok}
                else:
                    diff = table_diff(table, ref, "tiers")
                    paired = next((p for p, c in fr["continuous"]["arms"].items() if c == arm), None)
                    want = [TARGET] if paired else []
                    target_ok = True
                    if paired:
                        target_ok = table[TARGET]["tiers"] == [list(x) for x in
                                                               fr["arm_definitions"]["candidate_ladders"][paired]]
                    stops = sorted({table[l]["stop"] for l in LABELS})
                    ok = diff == want and target_ok and stops == [1.5] and row["stop_owners"] == ["unified_regime"] and all(table[l]["tiers"] for l in LABELS)
                    checks[arm] = {"labels_changed_vs_defaults": diff, "expected_changed": want,
                                   "target_ladder_ok": target_ok, "stops": stops, "stop_owner": row["stop_owners"],
                                   "ok": ok}
                if not checks[arm]["ok"]:
                    failures.append(f"b2 {arm}")
        cores = {}
        for arm in rows:
            for coin, tf in frame_coins(proto, frame):
                cand = arm_candidate(frame, arm, coin, tf)
                kind = "paired" if arm.startswith("paired_") else "cont"
                cores.setdefault((kind, coin, tf), set()).add(json.dumps(cand_core(cand), sort_keys=True, default=str))
        paired_same = all(len(v) == 1 for (kind, _c, _t), v in cores.items() if kind == "paired")
        checks["paired_arms_differ_only_in_close"] = paired_same
        if not paired_same:
            failures.append(f"{frame} paired arms differ outside the close")
        out["frames"][frame] = {"arms": rows, "checks": checks}
    if go_binary:
        for frame in sorted(proto["frames"]):
            for arm in frame_arm_names(proto, frame):
                path = arm_path(frame, arm)
                proc = subprocess.run([go_binary, "inspect", "--config", path, "--json", "--all"], capture_output=True,
                                      text=True, cwd=REPO, env={**os.environ, "HYPERLIQUID_SECRET_KEY": ""})
                rec = {"exit_code": proc.returncode, "stderr_tail": proc.stderr[-600:] if proc.returncode else ""}
                ok = proc.returncode == 0
                if ok:
                    printed = json.loads(proc.stdout)
                    cfg = load_json(path)
                    want = {s["id"]: s for s in cfg["strategies"]}
                    got = {s["id"]: s for s in printed}
                    same_ids = sorted(want) == sorted(got)
                    close_same = same_ids and all(
                        json.loads(json.dumps(got[i]["close_strategy"])) == json.loads(json.dumps(want[i][
                            "close_strategy"])) for i in want)
                    trail_same = True
                    if "trailing_stop_atr_mult_regime" in cfg["strategies"][0]:
                        trail_same = all(
                            sorted(x.split(" (from")[0] for x in got[i]["stop_loss"].get("detail") or []) == sorted(
                                f"trailing_stop_atr_mult_regime: {v['atr_multiple']:g}×ATR" for v in want[i][
                                    "trailing_stop_atr_mult_regime"]["trend_regime"].values()) for i in want)
                    rec.update({"strategies": len(got), "ids_match": same_ids, "close_strategy_preserved": close_same,
                                "trailing_regime_preserved": trail_same,
                                "go_stop_display_sources": sorted({got[i]["stop_loss"]["source"] for i in got}),
                                "allowed_regimes": sorted({json.dumps(got[i].get("allowed_regimes")) for i in got})})
                    ok = same_ids and close_same and trail_same
                rec["ok"] = ok
                out["go_inspect"][f"{frame}/{arm}.json"] = rec
                if not ok:
                    failures.append(f"go inspect {frame}/{arm}")
    out["failures"] = failures
    out["ok"] = not failures and bool(go_binary)
    write_json(ARM_VALIDATION_PATH, out)
    for f in failures:
        print("FAIL", f)
    print(f"wrote {rel(ARM_VALIDATION_PATH)}: {'ok' if out['ok'] else 'NOT ok'}")
    if failures:
        raise SystemExit(1)


def manifest_of(manifests: dict, group: str, tf: str) -> dict:
    return manifests[(group, tf)]


def window_key(proto: dict, group: str, window: str) -> str:
    return proto["native"]["window"] if group == "native" else window


def venue_absent_hours(inputs: dict, coin: str) -> set:
    meta = inputs["files"].get(f"data/proxy/{coin}_funding.csv.gz", {}).get("source_metadata") or {}
    return {int(h) for h, _obs in meta.get("venue_absent_hours") or []}


def funding_check(om, dataset: dict, win: dict, absent: set) -> dict:
    funding = om.load_funding(dataset)
    hours = set() if funding is None else {int(t) // HOUR_MS * HOUR_MS for t in funding["timestamp"].tolist()}
    start = win["start_ms"] // HOUR_MS * HOUR_MS
    want = range(start, win["end_ms"], HOUR_MS)
    missing = [h for h in want if h not in hours and h not in absent]
    absent_in = [h for h in want if h not in hours and h in absent]
    return {"expected_hours": len(want), "missing_hours": len(missing), "venue_absent_hours": len(absent_in),
            "first_missing_ms": missing[0] if missing else None, "complete": not missing}


def coverage() -> None:
    import offline_manifest as om
    import pandas as pd
    proto = protocol()
    frozen = verify_inputs()
    block_network()
    rows = []
    max_frac = proto["coverage_rule"]["max_missing_bar_fraction"]
    seen = set()
    for frame in sorted(proto["frames"]):
        for group, pairs in sorted(proto["frames"][frame]["groups"].items()):
            for coin, tf in pairs:
                windows = ["tail"] if group == "native" else ["train", "judge", "continuous"]
                for w in windows:
                    key = (group, coin, tf, w)
                    if key in seen:
                        continue
                    seen.add(key)
                    m = manifest_of(frozen["manifests"], group, tf)
                    row = {"group": group, "coin": coin, "interval": tf, "window": w,
                           "provenance": m["provenance"]["kind"]}
                    try:
                        ds = om.dataset_by_key(m, f"{coin} {tf}")
                        frame_df, win, cov = om.window_frame(m, ds, w)
                    except om.ManifestError as exc:
                        row.update({"status": "excluded", "reason": str(exc)})
                        rows.append(row)
                        continue
                    absent = venue_absent_hours(frozen["inputs"], coin) if group == "proxy" else set()
                    fcov = funding_check(om, ds, win, absent)
                    why = []
                    if cov["warmup_missing_bars"]:
                        why.append(f"{cov['warmup_missing_bars']} missing warm-up bars")
                    if cov["missing_bars"] > max_frac * cov["expected_bars"]:
                        why.append(f"{cov['missing_bars']} missing candles of {cov['expected_bars']}")
                    if not fcov["complete"]:
                        why.append(f"{fcov['missing_hours']} unpriced funding hours, first "
                                   f"{pd.Timestamp(fcov['first_missing_ms'], unit='ms')}")
                    row.update({"start": win["start"], "end": win["end"], "candles": cov, "funding": fcov,
                                "warmup_first_bar": str(frame_df.index[0]),
                                "status": "excluded" if why else "included",
                                "reason": "; ".join(why) if why else (
                                    "complete warm-up and funding" + (f"; {cov['missing_bars']} missing candles in the "
                                                                      "window scored as they are"
                                                                      if cov["missing_bars"] else ""))})
                    rows.append(row)
    rows.sort(key=lambda r: (r["group"], r["coin"], r["interval"], r["window"]))
    out = {"study": proto["study"], "protocol_sha256": sha256_file(PROTOCOL_PATH), "inputs": inputs_digest(),
           "network_calls": len(NETWORK_CALLS), "rows": rows}
    write_json(COVERAGE_PATH, out)
    for r in rows:
        print(f"{r['group']:<6} {r['coin']:<4} {r['interval']:<3} {r['window']:<10} {r['status']:<8} {r['reason']}")


def included(cov: dict, group: str, coin: str, tf: str, window: str) -> bool:
    w = "tail" if group == "native" else window
    return any(r["group"] == group and r["coin"] == coin and r["interval"] == tf and r["window"] == w
               and r["status"] == "included" for r in cov["rows"])


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


def labeller_digest() -> str:
    paths = ("backtest/regime_label_columns.py", "shared_tools/regime.py", "shared_tools/atr.py",
             "shared_strategies/open/adx_trend.py", "shared_strategies/open/indicators_core.py")
    return sha256_bytes(json.dumps({p: sha256_file(os.path.join(REPO, p)) for p in paths}, sort_keys=True).encode())


def study_backtester_class():
    from backtester import Backtester

    class StudyBacktester(Backtester):
        def _calculate_metrics(self, equity_df, trades, df, timeframe="1d"):
            self.study_equity = equity_df["equity"].copy()
            self.study_trades = list(trades)
            return super()._calculate_metrics(equity_df, trades, df, timeframe)

    return StudyBacktester


class FrameCtx:
    pass


def prepare_frame(reg, cand: dict, manifest: dict, coin: str, tf: str, wname: str, labels) -> FrameCtx:
    import offline_manifest as om
    from atr import ensure_atr_indicator, normalize_atr_method
    from closed_bar import bind_fixed_interval_params
    from regime_label_columns import attach_regime_label_columns, feature_label_kwargs
    name = cand["name"]
    strat = reg.STRATEGY_REGISTRY[name]
    params = bind_fixed_interval_params(strat["default_params"], cand.get("params"), tf)
    if strat.get("param_validator") is not None:
        reg.validate_params(name, params, strat["default_params"])
    strat_params = params if params is not None else strat["default_params"]
    dataset = om.dataset_by_key(manifest, f"{coin} {tf}")
    df, win, cov = om.window_frame(manifest, dataset, wname)
    df, fcov = om.attach_funding_cost(df, dataset, win)
    sig = reg.apply_strategy(name, df, strat_params)
    sig = ensure_atr_indicator(sig, method=normalize_atr_method(cand.get("atr_method") or "simple"))
    plan = cand["regime_label_windows"]
    sig, cols = attach_regime_label_columns(sig, plan, label_cache=labels)
    ctx = FrameCtx()
    ctx.coin, ctx.tf, ctx.wname = coin, tf, wname
    ctx.manifest, ctx.dataset, ctx.window, ctx.candle_cov, ctx.funding_cov = manifest, dataset, win, cov, fcov
    ctx.strat_params = strat_params
    ctx.plan = plan
    ctx.plan_key = json.dumps(plan, sort_keys=True, default=str)
    ctx.cols = cols
    ctx.feature = feature_label_kwargs(plan)
    ctx.indicator = sig
    ctx.sig = om.slice_window(sig, win)
    ctx.df = om.slice_window(df, win)
    ctx.pos = {ts: i for i, ts in enumerate(ctx.sig.index)}
    return ctx


def bt_kwargs(cand: dict, ctx: FrameCtx, cost_multiplier: float, capital: float) -> dict:
    import eval_windows as ew
    import offline_manifest as om
    if json.dumps(cand["regime_label_windows"], sort_keys=True, default=str) != ctx.plan_key:
        raise ValueError("arm label plan differs from the prepared frame")
    spec = om.execution_spec(ctx.manifest, ctx.dataset, cost_multiplier)
    windows_spec = cand.get("regime_windows_spec") or ctx.plan.get("windows_spec")
    kw = dict(
        initial_capital=capital, platform=ew.FEE_PLATFORM,
        open_strategy={"name": cand["name"], "params": dict(ctx.strat_params or {})},
        close_strategies=cand.get("close_strategies"),
        direction=cand.get("direction") or "long", invert_signal=False,
        stop_loss_atr_mult=cand.get("stop_loss_atr_mult"),
        trailing_stop_atr_mult=cand.get("trailing_stop_atr_mult"),
        profile_allocation=None, regime_enabled=True,
        regime_period=int(cand.get("regime_period") or 14),
        regime_adx_threshold=float(cand.get("regime_adx_threshold") or 20.0),
        allowed_regimes=cand.get("allowed_regimes"), regime_windows_spec=windows_spec,
        regime_gate_on_failure=cand.get("regime_gate_on_failure") or "open",
        regime_label_columns=ctx.cols, commission_pct=None, intrabar_resolution="ohlc_walk",
        comparison_mode=cand.get("comparison_mode"), atr_method=cand.get("atr_method") or "simple",
        execution_spec=spec,
    )
    for key, value in (ew.candidate_stop_kwargs(cand) or {}).items():
        if key in ("stop_loss_atr_mult", "trailing_stop_atr_mult"):
            if value != kw[key]:
                raise ValueError(f"stop_kwargs.{key}={value!r} disagrees with {kw[key]!r}")
            continue
        kw[key] = value
    kw.update(ctx.feature)
    return kw


def run_engine(cand: dict, ctx: FrameCtx, cost_multiplier: float, capital: float, frame=None):
    cls = study_backtester_class()
    bt = cls(**bt_kwargs(cand, ctx, cost_multiplier, capital))
    sig = ctx.sig if frame is None else frame
    results = bt.run(sig, strategy_name=cand["name"], symbol=ctx.coin, timeframe=ctx.tf,
                     params=ctx.strat_params, save=False, indicator_frame=ctx.indicator)
    return results, bt.study_trades, bt.study_equity


def positions_raw(trades: list) -> list:
    groups = OrderedDict()
    for t in trades:
        key = (str(t.entry_date), t.side)
        groups.setdefault(key, []).append(t)
    out = []
    for (entry, side), legs in groups.items():
        notional = sum(float(t.shares) * float(t.entry_price) for t in legs)
        pnl = sum(float(t.pnl) for t in legs)
        funding = sum(float(getattr(t, "funding_pnl", 0.0) or 0.0) for t in legs)
        fees = sum(float(t.entry_fee) + float(t.exit_fee) for t in legs)
        out.append({"entry": entry, "side": side, "exit": str(legs[-1].exit_date), "legs": len(legs),
                    "shares": sum(float(t.shares) for t in legs), "notional_usd": notional,
                    "pnl_usd": pnl, "funding_usd": funding, "fees_usd": fees,
                    "net_pct": (pnl + funding) / notional * 100.0 if notional > 0 else None,
                    "exit_reason": str(legs[-1].exit_reason), "exit_reasons": [str(t.exit_reason) for t in legs],
                    "bars_held": max(int(t.bars_held) for t in legs),
                    "entry_price": float(legs[0].entry_price)})
    return out


def replay(cand: dict, ctx: FrameCtx, cost: float, capital: float, sig_pos: int, side: str,
           lead: int = LEAD_BARS, horizon: int = HORIZON_BARS) -> dict:
    n = len(ctx.sig)
    lo = max(0, sig_pos - lead)
    hi = n if horizon is None else min(n, sig_pos + horizon + 1)
    fill_ts = str(ctx.sig.index[sig_pos + 1]) if sig_pos + 1 < n else None
    reruns = 0
    while True:
        one = ctx.sig.iloc[lo:hi].copy()
        col = one.columns.get_loc("signal")
        one.iloc[:, col] = 0
        one.iloc[sig_pos - lo, col] = -1 if side == "short" else 1
        results, trades, _eq = run_engine(cand, ctx, cost, capital, frame=one)
        pos = positions_raw(trades)
        if pos and pos[-1]["exit_reason"] == "end_of_data" and hi < n:
            hi = n
            reruns += 1
            continue
        break
    execution = results.get("execution") or {}
    rec = {"rejected_entries": execution.get("rejected_entry_count", 0),
           "skipped_partial_closes": execution.get("skipped_partial_close_count", 0),
           "stop_warmup_skipped": results.get("stop_warmup_skipped_entries", 0), "reruns": reruns}
    if len(pos) != 1 or pos[0]["entry"] != fill_ts or pos[0]["side"] != side or pos[0]["net_pct"] is None:
        rec.update({"matched": False, "why": f"{len(pos)} positions" + (
            f", first {pos[0]['entry']} {pos[0]['side']}" if pos else "")})
        return rec
    p = pos[0]
    rec.update({"matched": True, **{k: p[k] for k in ("net_pct", "pnl_usd", "funding_usd", "fees_usd",
                                                     "notional_usd", "shares", "legs", "exit", "exit_reason",
                                                     "exit_reasons", "bars_held")},
                "censored": p["exit_reason"] == "end_of_data"})
    return rec


def schedule_of(cand: dict, ctx: FrameCtx, capital: float) -> tuple:
    results, trades, _eq = run_engine(cand, ctx, 1.0, capital)
    pos = positions_raw(trades)
    import pandas as pd
    sched = []
    for p in pos:
        fill = ctx.pos.get(pd.Timestamp(p["entry"]))
        sched.append({"entry": p["entry"], "side": p["side"], "signal_pos": None if fill is None else fill - 1,
                      "signal_bar": None if fill is None or fill < 1 else str(ctx.sig.index[fill - 1]),
                      "free_run": {k: p[k] for k in ("net_pct", "exit", "exit_reason", "legs", "notional_usd")}})
    execution = results.get("execution") or {}
    diag = {"positions": len(pos), "close_records": len(trades),
            "long_positions": sum(1 for p in pos if p["side"] == "long"),
            "short_positions": sum(1 for p in pos if p["side"] == "short"),
            "rejected_entries": execution.get("rejected_entry_count", 0),
            "skipped_partial_closes": execution.get("skipped_partial_close_count", 0),
            "stop_warmup_skipped": results.get("stop_warmup_skipped_entries", 0),
            "exit_reasons": dict(sorted(_count([p["exit_reason"] for p in pos]).items()))}
    return sched, diag


def _count(xs) -> dict:
    out = {}
    for x in xs:
        out[x] = out.get(x, 0) + 1
    return out


_W = {}


def worker_init(work_dir: str) -> None:
    for p in (os.path.join(REPO, "shared_tools"), BACKTEST, os.path.join(REPO, "shared_strategies", "close")):
        if p not in sys.path:
            sys.path.insert(0, p)
    isolate_process(work_dir)
    from registry_loader import load_registry
    proto = protocol()
    _W["proto"] = proto
    _W["reg"] = load_registry(proto["open_strategy"]["registry"])
    _W["labels"] = DiskLabelCache(os.path.join(work_dir, "labels", labeller_digest()))
    _W["manifests"] = verify_inputs()["manifests"]
    _W["cands"] = {}


def worker_cand(frame: str, arm: str, coin: str, tf: str) -> dict:
    key = (frame, arm, coin, tf)
    if key not in _W["cands"]:
        _W["cands"][key] = arm_candidate(frame, arm, coin, tf)
    return _W["cands"][key]


def unit_id(u: dict) -> str:
    return "_".join([u["phase"], u["frame"], u["group"], u["coin"], u["interval"], u["window"]])


def unit_ctx(u: dict) -> FrameCtx:
    proto = _W["proto"]
    m = manifest_of(_W["manifests"], u["group"], u["interval"])
    cand = worker_cand(u["frame"], u["arms"][0], u["coin"], u["interval"])
    return prepare_frame(_W["reg"], cand, m, u["coin"], u["interval"], window_key(proto, u["group"], u["window"]),
                         _W["labels"])


def run_paired_unit(u: dict) -> dict:
    proto = _W["proto"]
    capital = proto["capital_usd"]
    ctx = unit_ctx(u)
    inc = worker_cand(u["frame"], u["arms"][0], u["coin"], u["interval"])
    sched, diag = schedule_of(inc, ctx, capital)
    rows = []
    for s in sched:
        row = {"entry": s["entry"], "side": s["side"], "signal_bar": s["signal_bar"], "free_run": s["free_run"],
               "arms": {}}
        for arm in u["arms"]:
            cand = worker_cand(u["frame"], arm, u["coin"], u["interval"])
            row["arms"][arm] = {}
            for cost in u["costs"]:
                mult = proto["costs"][cost]["cost_multiplier"]
                if s["signal_pos"] is None or s["signal_pos"] < 0:
                    row["arms"][arm][cost] = {"matched": False, "why": "no signal bar inside the window"}
                    continue
                row["arms"][arm][cost] = replay(cand, ctx, mult, capital, s["signal_pos"], s["side"])
        rows.append(row)
    rng = {"scored_first_bar": str(ctx.sig.index[0]), "scored_last_bar": str(ctx.sig.index[-1]),
           "scored_bars": int(len(ctx.sig)), "indicator_first_bar": str(ctx.indicator.index[0]),
           "candles": ctx.candle_cov, "funding": ctx.funding_cov}
    return {"unit": {k: v for k, v in u.items()}, "schedule": diag, "range": rng, "rows": rows}


def pack_ints(values) -> str:
    import numpy as np
    arr = np.asarray(values, dtype=np.int64)
    deltas = np.diff(np.concatenate([[0], arr]))
    return base64.b64encode(zlib.compress(deltas.astype("<i8").tobytes(), 9)).decode()


def unpack_ints(packed: str):
    import numpy as np
    return np.cumsum(np.frombuffer(zlib.decompress(base64.b64decode(packed)), dtype="<i8"))


def equity_record(trades: list, equity, capital: float, results: dict) -> dict:
    import numpy as np
    pnl = sum(float(t.pnl) for t in trades)
    funding = sum(float(getattr(t, "funding_pnl", 0.0) or 0.0) for t in trades)
    fees = sum(float(t.entry_fee) + float(t.exit_fee) for t in trades)
    final = capital + pnl + funding
    values = np.asarray(equity.to_numpy(dtype=float))
    stamps = [int(ts.value // 1_000_000) for ts in equity.index]
    terminal = final - float(values[-1])
    last_terminal = bool(trades) and str(trades[-1].exit_reason) == "end_of_data"
    path = {"bars": len(stamps), "unit": "1e-4 USD", "stamps_ms_zlib_b64": pack_ints(stamps),
            "equity_zlib_b64": pack_ints(np.round(values * 10_000).astype(np.int64))}
    return {
        "path": path,
        "final_equity": final, "terminal_settlement": terminal, "terminal_close": last_terminal,
        "pnl_net_fees_usd": pnl, "funding_usd": funding, "fees_usd": fees,
        "in_run_metrics": path_metrics(values, stamps, final, capital),
        "engine": {"final_capital": results.get("final_capital"), "total_return_pct": results.get("total_return_pct"),
                   "sharpe_ratio": results.get("sharpe_ratio"), "max_drawdown_pct": results.get("max_drawdown_pct"),
                   "total_trades": results.get("total_trades"), "total_funding_pnl": results.get("total_funding_pnl")},
        "reconciliation": {"engine_final_capital_minus_study": round(float(results.get("final_capital")) - final, 6),
                           "ok": abs(float(results.get("final_capital")) - final) <= 0.006},
    }


def unpack_path(path: dict) -> tuple:
    vals = unpack_ints(path["equity_zlib_b64"]) / 10_000.0
    stamps = [int(x) for x in unpack_ints(path["stamps_ms_zlib_b64"])]
    return vals, stamps


def path_metrics(values, stamps_ms: list, final: float, capital: float) -> dict:
    import numpy as np
    vals = np.concatenate([np.asarray(values, dtype=float), [final]])
    peak = np.maximum.accumulate(np.concatenate([[capital], vals]))[1:]
    dd = float(((vals - peak) / peak).min() * 100.0)
    days = OrderedDict()
    for ts, v in zip(stamps_ms, values):
        days[int(ts) // DAY_MS] = float(v)
    keys = list(days)
    closes = [days[k] for k in keys]
    closes[-1] = final
    series = np.asarray([capital] + closes, dtype=float)
    rets = series[1:] / series[:-1] - 1.0
    sharpe = float(rets.mean() / rets.std(ddof=1) * math.sqrt(365)) if len(rets) > 1 and rets.std(ddof=1) > 0 else 0.0
    return {"return_pct": (final - capital) / capital * 100.0, "net_pnl_usd": final - capital,
            "sharpe_daily": sharpe, "worst_drawdown_pct": dd, "days": len(keys)}


def run_continuous_unit(u: dict) -> dict:
    proto = _W["proto"]
    capital = proto["capital_usd"]
    ctx = unit_ctx(u)
    runs = {}
    for arm in u["arms"]:
        cand = worker_cand(u["frame"], arm, u["coin"], u["interval"])
        runs[arm] = {}
        for cost in u["costs"]:
            results, trades, equity = run_engine(cand, ctx, proto["costs"][cost]["cost_multiplier"], capital)
            rec = equity_record(trades, equity, capital, results)
            pos = positions_raw(trades)
            rec["positions"] = len(pos)
            rec["close_records"] = len(trades)
            rec["long_positions"] = sum(1 for p in pos if p["side"] == "long")
            rec["short_positions"] = sum(1 for p in pos if p["side"] == "short")
            rec["exit_reasons"] = dict(sorted(_count([p["exit_reason"] for p in pos]).items()))
            execution = results.get("execution") or {}
            rec["rejected_entries"] = execution.get("rejected_entry_count", 0)
            rec["skipped_partial_closes"] = execution.get("skipped_partial_close_count", 0)
            rec["trades_digest"] = sha256_bytes(json.dumps(
                [[str(t.entry_date), str(t.exit_date), t.side, repr(float(t.shares)), repr(float(t.pnl)),
                  str(t.exit_reason)] for t in trades]).encode())
            runs[arm][cost] = rec
    rng = {"scored_first_bar": str(ctx.sig.index[0]), "scored_last_bar": str(ctx.sig.index[-1]),
           "scored_bars": int(len(ctx.sig)), "indicator_first_bar": str(ctx.indicator.index[0]),
           "candles": ctx.candle_cov, "funding": ctx.funding_cov}
    return {"unit": {k: v for k, v in u.items()}, "range": rng, "runs": runs}


def label_task(u: dict) -> dict:
    t0 = time.time()
    unit_ctx(u)
    _W["labels"].trim()
    return {"unit": unit_id(u), "seconds": round(time.time() - t0, 1)}


def unit_task(task) -> dict:
    u, path, fp = task
    t0 = time.time()
    if u["kind"] == "paired":
        out = run_paired_unit(u)
    else:
        out = run_continuous_unit(u)
    out["fingerprint"] = fp
    out["seconds"] = round(time.time() - t0, 1)
    out["finished_utc"] = now_utc()
    _W["labels"].trim()
    os.makedirs(os.path.dirname(path), exist_ok=True)
    write_gz_json(path, out)
    return {"unit": unit_id(u), "seconds": out["seconds"]}


def unit_fingerprint(u: dict, lock: dict, extra: str = "") -> str:
    arms = {a: lock["arms_sha256"][f"{u['frame']}/{a}.json"] for a in u["arms"]}
    return sha256_bytes(json.dumps({"unit": u, "protocol": lock["protocol_sha256"], "code": lock["code_sha256"],
                                    "inputs": lock["inputs_sha256"], "arms": arms, "extra": extra},
                                   sort_keys=True).encode())


def unit_weight(u: dict) -> float:
    proto = protocol()
    w = proto["windows"].get(u["window"]) or {"start": "2026-05-03", "end": "2026-11-10"}
    days = (to_ms(w["end"]) - to_ms(w["start"])) / DAY_MS
    bars = days * {"30m": 48, "1h": 24, "4h": 6}[u["interval"]]
    return bars * (len(u["arms"]) * len(u["costs"]) + (3 if u["kind"] == "paired" else 0))


def run_units(units: list, phase: str, work_dir: str, workers: int, lock: dict, extra: str = "") -> dict:
    ckdir = os.path.join(work_dir, "ckpt", phase)
    tasks, todo = [], []
    for u in sorted(units, key=unit_weight, reverse=True):
        fp = unit_fingerprint(u, lock, extra)
        path = os.path.join(ckdir, unit_id(u) + ".json.gz")
        tasks.append((u, path, fp))
        fresh = False
        if os.path.exists(path):
            try:
                fresh = read_gz_json(path).get("fingerprint") == fp
            except (OSError, ValueError):
                fresh = False
        if not fresh:
            todo.append((u, path, fp))
    label_units = OrderedDict()
    for u, _p, _f in todo:
        label_units.setdefault((u["group"], u["coin"], u["interval"], u["window"]), u)
    print(f"[{phase}] {len(tasks)} units, {len(todo)} to score, {len(label_units)} label frames, {workers} workers",
          flush=True)
    started = time.time()
    if todo:
        ctx = multiprocessing.get_context("spawn")
        with ctx.Pool(processes=max(1, min(workers, len(todo))), initializer=worker_init,
                      initargs=(work_dir,)) as pool:
            lus = sorted(label_units.values(), key=unit_weight, reverse=True)
            for i, res in enumerate(pool.imap_unordered(label_task, lus), 1):
                print(f"[{phase}] labels {i}/{len(lus)} {res['unit']} {res['seconds']}s "
                      f"(elapsed {round(time.time() - started)}s)", flush=True)
            for i, res in enumerate(pool.imap_unordered(unit_task, todo), 1):
                print(f"[{phase}] {i}/{len(todo)} {res['unit']} {res['seconds']}s "
                      f"(elapsed {round(time.time() - started)}s)", flush=True)
    out = {}
    for u, path, fp in tasks:
        rec = read_gz_json(path)
        if rec.get("fingerprint") != fp:
            raise SystemExit(f"unit {unit_id(u)} has no fresh checkpoint")
        out[unit_id(u)] = rec
    return out


def locked_files(proto: dict) -> dict:
    out = {"protocol.json": sha256_file(PROTOCOL_PATH), "coverage_report.json": sha256_file(COVERAGE_PATH),
           "arm_validation.json": sha256_file(ARM_VALIDATION_PATH), "checks.json": sha256_file(CHECKS_PATH)}
    out.update(inputs_digest())
    for name, meta in sorted(load_json(INPUTS_PATH)["files"].items()):
        out[name] = meta["sha256"]
    for k, v in arms_sha256(proto).items():
        out[f"arms/{k}"] = v
    return out


def lock(revision: str) -> None:
    proto = protocol()
    build_manifests(check=True)
    build_arms(check=True)
    verify_inputs()
    for path in (COVERAGE_PATH, ARM_VALIDATION_PATH, CHECKS_PATH):
        if not os.path.exists(path):
            raise SystemExit(f"{os.path.basename(path)} is missing; the lock needs it")
    if not load_json(ARM_VALIDATION_PATH).get("ok"):
        raise SystemExit("arm_validation.json is not ok; the lock refuses")
    checks = load_json(CHECKS_PATH)
    if not checks.get("ok"):
        raise SystemExit("checks.json is not ok; the lock refuses")
    if load_json(ARM_VALIDATION_PATH)["arms_sha256"] != arms_sha256(proto):
        raise SystemExit("arm files changed after arm validation")
    if load_json(COVERAGE_PATH)["protocol_sha256"] != sha256_file(PROTOCOL_PATH):
        raise SystemExit("coverage_report.json was built from another protocol")
    if checks.get("code_sha256") != code_sha256() or checks.get("protocol_sha256") != sha256_file(PROTOCOL_PATH):
        raise SystemExit("checks.json was produced with other code or protocol bytes; rerun checks")
    history = []
    if os.path.exists(LOCK_PATH):
        held = load_json(LOCK_PATH)
        if not revision:
            raise SystemExit("protocol_lock.json exists; pass --revision '<reason>' to record a study revision")
        if os.path.exists(JUDGE_RESULTS_PATH) or os.path.exists(JUDGE_SAMPLES_PATH):
            raise SystemExit("judged results exist; a revision after judgment is refused")
        history = held.get("revisions", []) + [{"replaced_lock_sha256": sha256_file(LOCK_PATH),
                                                 "replaced_locked_at_utc": held["locked_at_utc"],
                                                 "reason": revision, "at_utc": now_utc()}]
    elif any(os.path.exists(p) for p in SCORING_OUTPUTS):
        raise SystemExit("scoring outputs exist; the protocol must be locked before any score is written")
    write_json(LOCK_PATH, {
        "study": proto["study"], "locked_at_utc": now_utc(), "source_commit_at_lock": git_head(),
        "protocol_sha256": sha256_file(PROTOCOL_PATH), "files_sha256": locked_files(proto),
        "arms_sha256": arms_sha256(proto), "inputs_sha256": inputs_digest(), "code_sha256": code_sha256(),
        "labeller_sha256": labeller_digest(), "revisions": history,
        "note": "written by run_study.py lock before any train, judge or continuous window was scored; scoring "
                "commands refuse unless this file and every locked file are committed and unchanged"})
    print(f"locked {rel(LOCK_PATH)}")


def lock_mismatches(held: dict, proto: dict) -> list:
    bad = []
    now = {"protocol_sha256": sha256_file(PROTOCOL_PATH), "files_sha256": locked_files(proto),
           "arms_sha256": arms_sha256(proto), "inputs_sha256": inputs_digest(), "code_sha256": code_sha256(),
           "labeller_sha256": labeller_digest()}
    for key, value in now.items():
        if isinstance(value, dict):
            for k in sorted(set(value) | set(held.get(key) or {})):
                if (held.get(key) or {}).get(k) != value.get(k):
                    bad.append(f"{key}:{k}")
        elif held.get(key) != value:
            bad.append(key)
    return bad


def lock_commit_status() -> dict:
    lock_rel = rel(LOCK_PATH)
    try:
        committed = git("show", f"HEAD:{lock_rel}")
    except subprocess.CalledProcessError:
        return {"ok": False, "why": "protocol_lock.json is not committed at HEAD"}
    if committed + "\n" != open(LOCK_PATH).read() and committed != open(LOCK_PATH).read():
        return {"ok": False, "why": "protocol_lock.json differs from HEAD"}
    paths = [rel(HERE)] + [p for p in CODE_FILES]
    dirty = git("status", "--porcelain", "--untracked-files=all", "--", *paths)
    allowed = {rel(p) for p in SCORING_OUTPUTS + (SCORING_LOG_PATH, REPORT_PATH, POST_CHECKS_PATH,
                                                  os.path.join(HERE, "README.md"),
                                                  os.path.join(HERE, "render_report.py"))}
    blocking = []
    for line in dirty.splitlines():
        path = line[3:].strip()
        if path in allowed or path.startswith(rel(os.path.join(HERE, "suggested_"))):
            continue
        blocking.append(line)
    if blocking:
        return {"ok": False, "why": "uncommitted changes to locked study or code files: " + "; ".join(blocking[:8])}
    lock_commit = git("log", "-1", "--format=%H", "--", lock_rel)
    return {"ok": True, "head": git_head(), "lock_commit": lock_commit}


def require_lock(scoring: bool = True) -> dict:
    if not os.path.exists(LOCK_PATH):
        raise SystemExit("protocol_lock.json is missing; run `run_study.py lock` first")
    held = load_json(LOCK_PATH)
    proto = protocol()
    build_manifests(check=True)
    build_arms(check=True)
    verify_inputs()
    bad = lock_mismatches(held, proto)
    if bad:
        raise SystemExit("locked bytes changed after the lock: " + ", ".join(bad[:12]))
    if scoring:
        status = lock_commit_status()
        if not status["ok"]:
            raise SystemExit(f"refusing to score: {status['why']}")
        held = dict(held, _commit=status)
    return held


def log_scoring(event: dict) -> None:
    log = load_json(SCORING_LOG_PATH) if os.path.exists(SCORING_LOG_PATH) else {"events": []}
    log["events"].append(event)
    write_json(SCORING_LOG_PATH, log)


def scoring_start(command: str, held: dict, extra: dict = None) -> dict:
    ev = {"command": command, "started_utc": now_utc(), "head": held["_commit"]["head"],
          "lock_commit": held["_commit"]["lock_commit"], "protocol_sha256": held["protocol_sha256"],
          "guarded_before": guarded_sha256(), **(extra or {})}
    first = not os.path.exists(SCORING_LOG_PATH)
    if first:
        ev["first_scoring"] = True
    log_scoring(ev)
    return ev


def scoring_finish(ev: dict, extra: dict = None) -> None:
    after = guarded_sha256()
    if after != ev["guarded_before"]:
        raise SystemExit("a shared cache or historical aggregate changed during scoring")
    log = load_json(SCORING_LOG_PATH)
    for e in log["events"]:
        if e["started_utc"] == ev["started_utc"] and e["command"] == ev["command"]:
            e.update({"finished_utc": now_utc(), "guarded_unchanged": True, "network_calls": len(NETWORK_CALLS),
                      **(extra or {})})
    write_json(SCORING_LOG_PATH, log)


def paired_units(proto: dict, cov: dict, phase: str, window: str, arms_by_frame: dict, costs: list,
                 groups: tuple) -> list:
    out = []
    for frame in sorted(proto["frames"]):
        if frame not in arms_by_frame:
            continue
        for group, pairs in sorted(proto["frames"][frame]["groups"].items()):
            if group not in groups:
                continue
            for coin, tf in pairs:
                if not included(cov, group, coin, tf, window):
                    continue
                w = "tail" if group == "native" else window
                out.append({"kind": "paired", "phase": phase, "frame": frame, "group": group, "coin": coin,
                            "interval": tf, "window": w, "arms": list(arms_by_frame[frame]), "costs": list(costs)})
    return out


def flatten_samples(units: dict, proto: dict) -> list:
    rows = []
    for uid in sorted(units):
        rec = units[uid]
        u = rec["unit"]
        for r in rec["rows"]:
            rows.append({"phase": u["phase"], "frame": u["frame"], "group": u["group"], "coin": u["coin"],
                         "interval": u["interval"], "window": u["window"], "dataset": f"{u['coin']} {u['interval']}",
                         **r})
    return rows


def unit_summaries(units: dict) -> dict:
    out = {}
    for uid in sorted(units):
        rec = units[uid]
        arms = {}
        for arm in rec["unit"]["arms"]:
            for cost in rec["unit"]["costs"]:
                recs = [r["arms"][arm][cost] for r in rec["rows"]]
                matched = [x for x in recs if x.get("matched")]
                arms.setdefault(arm, {})[cost] = {
                    "scheduled": len(recs), "matched": len(matched),
                    "unmatched": [{"entry": r["entry"], "why": r["arms"][arm][cost].get("why")}
                                  for r in rec["rows"] if not r["arms"][arm][cost].get("matched")],
                    "close_records": sum(x["legs"] for x in matched),
                    "censored": sum(1 for x in matched if x.get("censored")),
                    "reruns": sum(x.get("reruns", 0) for x in recs),
                    "skipped_partial_closes": sum(x.get("skipped_partial_closes", 0) for x in recs),
                    "rejected_entries": sum(x.get("rejected_entries", 0) for x in recs)}
        out[uid] = {"unit": rec["unit"], "schedule": rec["schedule"], "range": rec["range"], "arms": arms,
                    "seconds": rec.get("seconds"), "fingerprint": rec["fingerprint"]}
    return out


def pooled_mean(rows: list, arm: str, base: str, cost: str) -> tuple:
    ds = [r["arms"][arm][cost]["net_pct"] - r["arms"][base][cost]["net_pct"] for r in rows
          if r["arms"][arm][cost].get("matched") and r["arms"][base][cost].get("matched")]
    return (sum(ds) / len(ds) if ds else None), len(ds)


def select_cmd(work_dir: str, workers: int) -> None:
    proto = protocol()
    held = require_lock()
    if os.path.exists(JUDGE_RESULTS_PATH) or os.path.exists(JUDGE_SAMPLES_PATH):
        raise SystemExit("judged results exist; selection cannot be rewritten")
    if os.path.exists(SELECTION_PATH) and load_json(SELECTION_PATH)["lock_sha256"] == sha256_file(LOCK_PATH):
        raise SystemExit("selection.json exists for this lock; it is written once")
    isolate_process(work_dir)
    probe = network_probe()
    cov = load_json(COVERAGE_PATH)
    ev = scoring_start("select", held)
    arms = {f: [proto["frames"][f]["paired"]["incumbent"], proto["frames"][f]["paired"]["arms"]["primary"],
                *proto["frames"][f]["paired"]["arms"]["neighbours"]] for f in proto["frames"]}
    units = paired_units(proto, cov, "train", "train", arms, proto["paired_method"]["training_costs"], ("proxy",))
    res = run_units(units, "train", work_dir, workers, held)
    samples = flatten_samples(res, proto)
    picks = {}
    for frame in sorted(proto["frames"]):
        fr = proto["frames"][frame]
        rows = [r for r in samples if r["frame"] == frame]
        inc = fr["paired"]["incumbent"]
        scores = {}
        for arm in [fr["paired"]["arms"]["primary"], *fr["paired"]["arms"]["neighbours"]]:
            mean, n = pooled_mean(rows, arm, inc, "base")
            scores[arm] = {"pooled_mean_delta_pct": mean, "matched_positions": n}
        neigh = fr["paired"]["arms"]["neighbours"]
        widen = fr["arm_definitions"]["widening"]
        ranked = sorted(neigh, key=lambda a: (-(scores[a]["pooled_mean_delta_pct"]
                                                if scores[a]["pooled_mean_delta_pct"] is not None else -math.inf),
                                              widen[a]))
        a, b = ranked
        if (scores[a]["pooled_mean_delta_pct"] is not None and scores[b]["pooled_mean_delta_pct"] is not None
                and abs(scores[a]["pooled_mean_delta_pct"] - scores[b]["pooled_mean_delta_pct"]) <= 1e-12):
            a = min(neigh, key=lambda x: widen[x])
        top = scores[a]
        pick = a if (top["pooled_mean_delta_pct"] is not None and top["pooled_mean_delta_pct"] > 0
                     and top["matched_positions"] >= 30) else None
        picks[frame] = {"scores": scores, "ranked": ranked, "top_neighbour": a, "shortlisted_neighbour": pick,
                        "judged": [fr["paired"]["arms"]["primary"]] + ([pick] if pick else []),
                        "unjudged": [x for x in neigh if x != pick]}
    write_gz_json(TRAIN_SAMPLES_PATH, samples)
    write_json(TRAIN_RESULTS_PATH, {"window": proto["windows"]["train"], "units": unit_summaries(res),
                                    "samples_sha256": sha256_file(TRAIN_SAMPLES_PATH)})
    write_json(SELECTION_PATH, {"study": proto["study"], "lock_sha256": sha256_file(LOCK_PATH),
                                "written_utc": now_utc(), "head": held["_commit"]["head"],
                                "rule": proto["selection"]["rule"], "frames": picks, "network_probe": probe,
                                "train_samples_sha256": sha256_file(TRAIN_SAMPLES_PATH)})
    scoring_finish(ev, {"selection_sha256": sha256_file(SELECTION_PATH)})
    for frame, p in picks.items():
        print(frame, json.dumps(p["scores"]), "->", p["judged"])


def require_selection() -> dict:
    if not os.path.exists(SELECTION_PATH):
        raise SystemExit("selection.json is missing; run select first")
    sel = load_json(SELECTION_PATH)
    if sel["lock_sha256"] != sha256_file(LOCK_PATH):
        raise SystemExit("selection.json belongs to another lock")
    if sha256_file(TRAIN_SAMPLES_PATH) != sel["train_samples_sha256"]:
        raise SystemExit("samples_train.json.gz changed after selection")
    return sel


def judged_arms(proto: dict, sel: dict, frame: str) -> list:
    fr = proto["frames"][frame]
    judged = sel["frames"][frame]["judged"]
    allowed = {fr["paired"]["arms"]["primary"], *fr["paired"]["arms"]["neighbours"]}
    if fr["paired"]["arms"]["primary"] not in judged or not set(judged) <= allowed or len(judged) > 2:
        raise SystemExit(f"{frame}: selection {judged} breaks the frozen rule")
    return [fr["paired"]["incumbent"]] + judged


def judge_cmd(work_dir: str, workers: int) -> None:
    proto = protocol()
    held = require_lock()
    sel = require_selection()
    if os.path.exists(JUDGE_SAMPLES_PATH):
        prior = load_json(JUDGE_RESULTS_PATH)
        if prior.get("selection_sha256") != sha256_file(SELECTION_PATH):
            raise SystemExit("judged results exist for another selection")
    isolate_process(work_dir)
    probe = network_probe()
    cov = load_json(COVERAGE_PATH)
    sel_sha = sha256_file(SELECTION_PATH)
    ev = scoring_start("judge", held, {"selection_sha256": sel_sha})
    arms = {f: judged_arms(proto, sel, f) for f in proto["frames"]}
    units = paired_units(proto, cov, "judge", "judge", arms, proto["paired_method"]["judgment_costs"],
                         ("proxy", "native"))
    res = run_units(units, "judge", work_dir, workers, held, extra=sel_sha)
    samples = flatten_samples(res, proto)
    write_gz_json(JUDGE_SAMPLES_PATH, samples)
    write_json(JUDGE_RESULTS_PATH, {"window": proto["windows"]["judge"], "native_windows": proto["native"]["windows"],
                                    "selection_sha256": sel_sha, "units": unit_summaries(res),
                                    "samples_sha256": sha256_file(JUDGE_SAMPLES_PATH), "network_probe": probe})
    scoring_finish(ev, {"judge_samples_sha256": sha256_file(JUDGE_SAMPLES_PATH)})
    print(f"wrote {rel(JUDGE_RESULTS_PATH)} ({len(samples)} scheduled positions)")


def continuous_arms(proto: dict, sel: dict, frame: str) -> list:
    fr = proto["frames"][frame]
    arms = [fr["continuous"]["incumbent"]] + [fr["continuous"]["arms"][a] for a in sel["frames"][frame]["judged"]]
    if fr["continuous"].get("equivalence"):
        arms.append(fr["continuous"]["equivalence"]["historical"])
    return arms


def continuous_cmd(work_dir: str, workers: int) -> None:
    proto = protocol()
    held = require_lock()
    sel = require_selection()
    isolate_process(work_dir)
    probe = network_probe()
    cov = load_json(COVERAGE_PATH)
    sel_sha = sha256_file(SELECTION_PATH)
    ev = scoring_start("continuous", held, {"selection_sha256": sel_sha})
    units = []
    for frame in sorted(proto["frames"]):
        arms = continuous_arms(proto, sel, frame)
        for group, pairs in sorted(proto["frames"][frame]["groups"].items()):
            for coin, tf in pairs:
                w = "tail" if group == "native" else "continuous"
                if not included(cov, group, coin, tf, "continuous"):
                    continue
                units.append({"kind": "continuous", "phase": "continuous", "frame": frame, "group": group,
                              "coin": coin, "interval": tf, "window": w, "arms": arms,
                              "costs": ["base", "stress"]})
    res = run_units(units, "continuous", work_dir, workers, held, extra=sel_sha)
    summary, equity = {}, {}
    for uid in sorted(res):
        rec = res[uid]
        runs = {}
        for arm, by_cost in rec["runs"].items():
            runs[arm] = {}
            for cost, r in by_cost.items():
                key = f"{uid}|{arm}|{cost}"
                equity[key] = {"path": r["path"], "final_equity": r["final_equity"]}
                runs[arm][cost] = {k: v for k, v in r.items() if k != "path"}
        summary[uid] = {"unit": rec["unit"], "range": rec["range"], "runs": runs, "seconds": rec.get("seconds")}
    write_gz_json(EQUITY_PATH, equity)
    write_json(CONTINUOUS_PATH, {"selection_sha256": sel_sha, "units": summary, "network_probe": probe,
                                 "equity_sha256": sha256_file(EQUITY_PATH)})
    scoring_finish(ev, {"continuous_sha256": sha256_file(CONTINUOUS_PATH)})
    print(f"wrote {rel(CONTINUOUS_PATH)} ({len(summary)} units)")


def equity_series(entry: dict) -> tuple:
    return unpack_path(entry["path"])


def block_of(entry: str, start_ms: int) -> int:
    import pandas as pd
    ts = int(pd.Timestamp(entry).value // 1_000_000)
    return int((ts - start_ms) // (7 * DAY_MS))


def holm(pvals: dict, alpha: float) -> dict:
    items = sorted(pvals.items(), key=lambda kv: (kv[1], kv[0]))
    m = len(items)
    out = {}
    stop = False
    for i, (k, p) in enumerate(items):
        thresh = alpha / (m - i)
        rej = (not stop) and p <= thresh
        if not rej:
            stop = True
        out[k] = {"p": p, "threshold": thresh, "rejected": rej}
    return out


def boot_draws(nblocks: int, resamples: int, seed: int):
    import numpy as np
    return np.random.default_rng(seed).integers(0, nblocks, size=(resamples, nblocks))


def block_sums(rows: list, nblocks: int):
    import numpy as np
    s = np.zeros(nblocks)
    n = np.zeros(nblocks)
    for b, d in rows:
        s[b] += d
        n[b] += 1
    return s, n


def centered_p(s, n, draws, harm: bool) -> dict:
    import numpy as np
    tot = n.sum()
    if tot == 0:
        return {"mean": None, "p": None, "valid_draws": 0}
    t = float(s.sum() / tot)
    sb = s[draws].sum(axis=1)
    nb = n[draws].sum(axis=1)
    ok = nb > 0
    tb = sb[ok] / nb[ok]
    if harm:
        hits = int(((tb - t) <= t).sum())
    else:
        hits = int(((tb - t) >= t).sum())
    return {"mean": t, "p": (1 + hits) / (1 + int(ok.sum())), "valid_draws": int(ok.sum())}


def verdict_units(proto: dict) -> list:
    out = []
    for frame in sorted(proto["frames"]):
        for group in sorted(proto["frames"][frame]["groups"]):
            out.append((frame, group, proto["frames"][frame]["direction"]))
    return out


def window_bounds(proto: dict, group: str, interval: str = None) -> tuple:
    if group == "native":
        s, e = proto["native"]["windows"][interval]
        return to_ms(s), to_ms(e)
    w = proto["windows"]["judge"]
    return to_ms(w["start"]), to_ms(w["end"])


def decide(proto: dict, samples: list, sel: dict, cont: dict, equity: dict) -> dict:
    st = proto["statistics"]
    floors = st["sample_floors"]
    alpha = st["alpha"]
    out = {"units": {}, "family": {}}
    decision_p = {}
    pending = {}
    for frame, group, direction in verdict_units(proto):
        fr = proto["frames"][frame]
        inc = fr["paired"]["incumbent"]
        searched = [fr["paired"]["arms"]["primary"], *fr["paired"]["arms"]["neighbours"]]
        judged = sel["frames"][frame]["judged"]
        rows = [r for r in samples if r["frame"] == frame and r["group"] == group]
        ukey = f"{frame}/{group}/{direction}"
        if group == "native":
            starts = {r["interval"]: window_bounds(proto, group, r["interval"]) for r in rows}
            start_ms = min((s for s, _e in starts.values()), default=to_ms("2026-05-03"))
            end_ms = max((e for _s, e in starts.values()), default=to_ms("2026-11-10"))
        else:
            start_ms, end_ms = window_bounds(proto, group)
        nblocks = int(math.ceil((end_ms - start_ms) / (7 * DAY_MS)))
        draws = boot_draws(nblocks, st["resamples"], st["seed"])
        datasets = sorted({r["dataset"] for r in rows})
        sides = ["long", "short"] if direction == "both" else [direction]
        arms_out = {}
        for arm in searched:
            if arm not in judged:
                decision_p[f"{ukey}/{arm}"] = 1.0
                arms_out[arm] = {"judged": False}
                continue
            per_cost = {}
            for cost in proto["paired_method"]["judgment_costs"]:
                pairs = []
                scheduled = len(rows)
                for r in rows:
                    a, b = r["arms"][arm][cost], r["arms"][inc][cost]
                    if a.get("matched") and b.get("matched"):
                        pairs.append((r, a["net_pct"] - b["net_pct"]))
                allrows = [(block_of(r["entry"], start_ms), d) for r, d in pairs]
                s, n = block_sums(allrows, nblocks)
                test = centered_p(s, n, draws, harm=False)
                eff_blocks = int((n > 0).sum())
                harm = {}
                for ds in datasets:
                    for side in sides:
                        sub = [(block_of(r["entry"], start_ms), d) for r, d in pairs
                               if r["dataset"] == ds and (direction != "both" or r["side"] == side)]
                        hkey = f"{ds}" + (f" {side}" if direction == "both" else "")
                        if len(sub) < floors["harm_min_positions"]:
                            harm[hkey] = {"n": len(sub), "tested": False,
                                          "mean": (sum(d for _b, d in sub) / len(sub)) if sub else None}
                            continue
                        hs, hn = block_sums(sub, nblocks)
                        h = centered_p(hs, hn, draws, harm=True)
                        harm[hkey] = {"n": len(sub), "tested": True, "mean": h["mean"], "p": h["p"]}
                tested = {k: v["p"] for k, v in harm.items() if v["tested"]}
                hc = holm(tested, alpha) if tested else {}
                for k in hc:
                    harm[k]["holm"] = hc[k]
                harmed = sorted(k for k, v in hc.items() if v["rejected"] and harm[k]["mean"] < 0)
                side_att = {}
                for side in ["long", "short"]:
                    ds_ = [d for r, d in pairs if r["side"] == side]
                    side_att[side] = {"n": len(ds_), "mean": (sum(ds_) / len(ds_)) if ds_ else None}
                per_ds = {}
                for ds in datasets:
                    d_ = [d for r, d in pairs if r["dataset"] == ds]
                    sched_ds = sum(1 for r in rows if r["dataset"] == ds)
                    per_ds[ds] = {"scheduled": sched_ds, "matched": len(d_),
                                  "mean": (sum(d_) / len(d_)) if d_ else None,
                                  "incumbent_mean_net_pct": _mean([r["arms"][inc][cost]["net_pct"] for r, _d in pairs
                                                                   if r["dataset"] == ds]),
                                  "candidate_mean_net_pct": _mean([r["arms"][arm][cost]["net_pct"] for r, _d in pairs
                                                                   if r["dataset"] == ds])}
                coverage_ratio = (len(pairs) / scheduled) if scheduled else 0.0
                floors_ok = (len(pairs) >= floors["min_matched_positions"]
                             and eff_blocks >= floors["min_effective_blocks"]
                             and coverage_ratio >= floors["min_match_coverage"])
                per_cost[cost] = {"scheduled": scheduled, "matched": len(pairs), "match_coverage": coverage_ratio,
                                  "effective_blocks": eff_blocks, "blocks": nblocks, "mean_delta_pct": test["mean"],
                                  "p_improve": test["p"], "valid_draws": test["valid_draws"], "harm": harm,
                                  "harmed": harmed, "floors_ok": floors_ok, "sides": side_att, "datasets": per_ds,
                                  "incumbent_mean_net_pct": _mean([r["arms"][inc][cost]["net_pct"] for r, _d in pairs]),
                                  "candidate_mean_net_pct": _mean([r["arms"][arm][cost]["net_pct"] for r, _d in pairs])}
            ps = [per_cost[c]["p_improve"] for c in per_cost]
            dp = max(ps) if all(p is not None for p in ps) else 1.0
            decision_p[f"{ukey}/{arm}"] = dp
            arms_out[arm] = {"judged": True, "costs": per_cost, "decision_p": dp,
                             "continuous": continuous_gate(proto, cont, equity, frame, group, arm)}
        pending[ukey] = {"frame": frame, "group": group, "direction": direction, "arms": arms_out,
                         "datasets": datasets, "scheduled": len(rows), "blocks": nblocks,
                         "block_start_ms": start_ms}
    fam = holm(decision_p, alpha)
    out["family"] = {"m": len(decision_p), "alpha": alpha, "tests": fam}
    for ukey, u in pending.items():
        fr = proto["frames"][u["frame"]]
        primary = fr["paired"]["arms"]["primary"]
        passing = []
        for arm, a in u["arms"].items():
            if not a["judged"]:
                continue
            reasons = []
            for cost, c in a["costs"].items():
                if not c["floors_ok"]:
                    reasons.append(f"{cost}: sample floor or match coverage missed")
                if c["mean_delta_pct"] is None or c["mean_delta_pct"] <= 0:
                    reasons.append(f"{cost}: pooled mean delta not above 0")
                if c["harmed"]:
                    reasons.append(f"{cost}: significant negative " + ", ".join(c["harmed"]))
            if not fam[f"{ukey}/{arm}"]["rejected"]:
                reasons.append(f"decision p {a['decision_p']:.4g} not rejected by Holm")
            if not a["continuous"]["pass"]:
                reasons.append("continuous gate: " + a["continuous"]["why"])
            a["holm"] = fam[f"{ukey}/{arm}"]
            a["passes"] = not reasons
            a["fails"] = reasons
            if not reasons:
                passing.append(arm)
        prim = u["arms"].get(primary) or {}
        adequate = bool(u["datasets"]) and prim.get("judged") and all(
            c["floors_ok"] for c in prim.get("costs", {}).values())
        if passing:
            verdict = "positive"
        elif adequate:
            verdict = "negative"
        else:
            verdict = "inconclusive"
        best = min(passing, key=lambda a: (u["arms"][a]["decision_p"], a)) if passing else None
        u.update({"verdict": verdict, "passing_arms": passing, "suggested_arm": best, "primary_adequate": adequate})
        out["units"][ukey] = u
    return out


def _mean(xs: list):
    xs = [x for x in xs if x is not None]
    return sum(xs) / len(xs) if xs else None


def continuous_metrics(proto: dict, cont: dict, equity: dict, frame: str, group: str, arm: str) -> dict:
    capital = proto["capital_usd"]
    out = {}
    for uid, u in sorted(cont["units"].items()):
        if u["unit"]["frame"] != frame or u["unit"]["group"] != group or arm not in u["runs"]:
            continue
        ds = f"{u['unit']['coin']} {u['unit']['interval']}"
        for cost in ("base", "stress"):
            entry = equity[f"{uid}|{arm}|{cost}"]
            vals, stamps = equity_series(entry)
            m = path_metrics(vals, stamps, entry["final_equity"], capital)
            r = u["runs"][arm][cost]
            m.update({"positions": r["positions"], "close_records": r["close_records"],
                      "fees_usd": r["fees_usd"], "funding_usd": r["funding_usd"],
                      "terminal_settlement_usd": r["terminal_settlement"], "terminal_close": r["terminal_close"],
                      "engine": r["engine"], "reconciled": r["reconciliation"]["ok"],
                      "first_bar": u["range"]["scored_first_bar"], "last_bar": u["range"]["scored_last_bar"]})
            out.setdefault(cost, {})[ds] = m
    return out


def continuous_gate(proto: dict, cont: dict, equity: dict, frame: str, group: str, arm: str) -> dict:
    fr = proto["frames"][frame]
    carm = fr["continuous"]["arms"][arm]
    inc = continuous_metrics(proto, cont, equity, frame, group, fr["continuous"]["incumbent"])
    cand = continuous_metrics(proto, cont, equity, frame, group, carm)
    why = []
    res = {"arm": carm, "costs": {}}
    if not inc or not cand:
        return {"pass": False, "why": "no continuous run", **res}
    for cost in ("base", "stress"):
        dsets = sorted(set(inc.get(cost, {})) & set(cand.get(cost, {})))
        ipnl = sum(inc[cost][d]["net_pnl_usd"] for d in dsets)
        cpnl = sum(cand[cost][d]["net_pnl_usd"] for d in dsets)
        dd_worse = sorted(d for d in dsets if cand[cost][d]["worst_drawdown_pct"]
                          < inc[cost][d]["worst_drawdown_pct"] - 2.0)
        res["costs"][cost] = {"datasets": dsets, "incumbent_net_pnl_usd": ipnl, "candidate_net_pnl_usd": cpnl,
                              "denominator_usd": proto["capital_usd"] * len(dsets),
                              "incumbent_pooled_return_pct": ipnl / (proto["capital_usd"] * len(dsets)) * 100
                              if dsets else None,
                              "candidate_pooled_return_pct": cpnl / (proto["capital_usd"] * len(dsets)) * 100
                              if dsets else None,
                              "drawdown_worse_by_more_than_2pp": dd_worse}
        if not dsets:
            why.append(f"{cost}: no datasets")
        if cpnl <= ipnl:
            why.append(f"{cost}: candidate net PnL {cpnl:.2f} not above incumbent {ipnl:.2f}")
        if dd_worse:
            why.append(f"{cost}: deeper drawdown in " + ", ".join(dd_worse))
    return {"pass": not why, "why": "; ".join(why), **res}


def verdict_cmd() -> None:
    proto = protocol()
    require_lock(scoring=False)
    sel = require_selection()
    judge = load_json(JUDGE_RESULTS_PATH)
    if judge["samples_sha256"] != sha256_file(JUDGE_SAMPLES_PATH) or judge["selection_sha256"] != sha256_file(
            SELECTION_PATH):
        raise SystemExit("judged samples or selection changed")
    cont = load_json(CONTINUOUS_PATH)
    if cont["equity_sha256"] != sha256_file(EQUITY_PATH) or cont["selection_sha256"] != sha256_file(SELECTION_PATH):
        raise SystemExit("continuous files changed")
    samples = read_gz_json(JUDGE_SAMPLES_PATH)
    equity = read_gz_json(EQUITY_PATH)
    out = decide(proto, samples, sel, cont, equity)
    out["equivalence"] = equivalence_report(proto, cont)
    out["continuous"] = {}
    for frame, group, _d in verdict_units(proto):
        fr = proto["frames"][frame]
        arms = continuous_arms(proto, sel, frame)
        out["continuous"][f"{frame}/{group}"] = {a: continuous_metrics(proto, cont, equity, frame, group, a)
                                                 for a in arms}
    out["inputs"] = {"judge_samples_sha256": sha256_file(JUDGE_SAMPLES_PATH),
                     "continuous_sha256": sha256_file(CONTINUOUS_PATH), "selection_sha256": sha256_file(SELECTION_PATH),
                     "lock_sha256": sha256_file(LOCK_PATH)}
    for name in sorted(os.listdir(HERE)):
        if name.startswith("suggested_") and name.endswith(".json"):
            os.remove(os.path.join(HERE, name))
    for ukey, u in sorted(out["units"].items()):
        if u["verdict"] != "positive":
            continue
        frame, group, _d = ukey.split("/")
        carm = proto["frames"][frame]["continuous"]["arms"][u["suggested_arm"]]
        cfg = load_json(arm_path(frame, carm))
        groups = {tuple(p) for p in proto["frames"][frame]["groups"][group]}
        cfg["strategies"] = [s for s in cfg["strategies"] if (s["args"][1], s["args"][2]) in groups]
        cm = out["continuous"][f"{frame}/{group}"][carm]
        losses = {cost: sorted(d for d, m in cm.get(cost, {}).items() if m["net_pnl_usd"] < 0) for cost in cm}
        cfg["_study"] = {"issue": 2018, "unit": ukey, "arm": u["suggested_arm"], "continuous_arm": carm,
                         "status": "suggest-only offline artifact; not a live or paper config and not a default",
                         "absolute_loss_datasets": losses,
                         "label": "relative improvement over the incumbent exit; absolute losses listed here remain "
                                  "losses"}
        path = os.path.join(HERE, f"suggested_{frame}_{group}_config.json")
        write_json(path, cfg)
        u["suggested_config"] = os.path.basename(path)
    write_json(VERDICT_PATH, out)
    for ukey, u in sorted(out["units"].items()):
        print(ukey, u["verdict"], u["passing_arms"])


def equivalence_report(proto: dict, cont: dict) -> dict:
    out = {}
    for frame in sorted(proto["frames"]):
        eq = proto["frames"][frame]["continuous"].get("equivalence")
        if not eq:
            continue
        rows = {}
        for uid, u in sorted(cont["units"].items()):
            if u["unit"]["frame"] != frame:
                continue
            for cost in ("base", "stress"):
                a = u["runs"][eq["historical"]][cost]
                b = u["runs"][eq["unified"]][cost]
                rows[f"{uid}|{cost}"] = {"identical_trades": a["trades_digest"] == b["trades_digest"],
                                         "historical_final": a["final_equity"], "unified_final": b["final_equity"]}
        out[frame] = {"all_identical": all(r["identical_trades"] for r in rows.values()), "rows": rows}
    return out


def checks_cmd(work_dir: str) -> None:
    import eval_windows as ew
    import offline_manifest as om
    from registry_loader import load_registry
    proto = protocol()
    if os.path.exists(LOCK_PATH):
        raise SystemExit("checks run before the lock; use checks-post afterwards")
    guarded_before = guarded_sha256()
    frozen = verify_inputs()
    os.makedirs(work_dir, exist_ok=True)
    isolate_process(work_dir)
    out = {"protocol_sha256": sha256_file(PROTOCOL_PATH), "code_sha256": code_sha256(), "window": "parity",
           "measured_at_utc": now_utc(), "source_commit": git_head(), "network_probe": network_probe(), "cases": {}}
    reg = load_registry(proto["open_strategy"]["registry"])
    labels = DiskLabelCache(os.path.join(work_dir, "labels", labeller_digest()))
    capital = proto["capital_usd"]
    cases = out["cases"]
    failures = []

    def ctx_for(frame, arm, coin, tf):
        cand = arm_candidate(frame, arm, coin, tf)
        m = frozen["manifests"][("proxy", tf)]
        return cand, prepare_frame(reg, cand, m, coin, tf, "parity", labels)

    for frame, arm, coin, tf in (("b2", "paired_incumbent", "BTC", "1h"), ("b2", "cont_incumbent", "ETH", "4h"),
                                 ("ratchet", "cont_incumbent", "BTC", "30m"), ("ratchet", "paired_r_w150", "SOL", "1h")):
        cand, ctx = ctx_for(frame, arm, coin, tf)
        results, trades, equity = run_engine(cand, ctx, 1.0, capital)
        leg = ew.run_candidate_leg(reg, cand, coin, tf, None, capital, keep_trades=True,
                                   manifest_ctx={"manifest": ctx.manifest, "window": "parity", "cost_multiplier": 1.0},
                                   regime_label_cache=labels)
        mine = {"return_pct": results["total_return_pct"], "sharpe": results["sharpe_ratio"],
                "max_dd_pct": results["max_drawdown_pct"], "trades": results["total_trades"]}
        theirs = {k: leg[k] for k in mine}
        mine_pos = [[p["entry"], p["side"], p["pnl_usd"] + p["funding_usd"], p["legs"]] for p in positions_raw(trades)]
        their_pos = [[p["entry_date"], p["side"], p["net_pnl"], p["closes"]] for p in leg["execution"]["position_list"]]
        same_keys = len(mine_pos) == len(their_pos) and all(
            a[0] == b[0] and a[1] == b[1] and a[3] == b[3] for a, b in zip(mine_pos, their_pos))
        diffs = [abs(a[2] - b[2]) for a, b in zip(mine_pos, their_pos)]
        within = all(d <= 0.005 * a[3] + 2e-6 for d, a in zip(diffs, mine_pos))
        ok = mine == theirs and same_keys and within
        cases[f"runner_parity {frame}/{arm} {coin} {tf}"] = {
            "study_runner": mine, "eval_windows_run_leg": theirs, "positions": len(mine_pos),
            "entries_sides_close_records_equal": same_keys, "max_abs_position_pnl_diff_usd": max(diffs, default=0.0),
            "note": "eval_windows sums the cent-rounded trade pnl of results['trades']; the study sums the unrounded "
                    "engine trade objects, so each close record may differ by at most half a cent",
            "ok": ok}
        if not ok:
            failures.append(f"runner_parity {frame}/{arm} {coin} {tf}")
        rec = equity_record(trades, equity, capital, results)
        pvals, pstamps = unpack_path(rec["path"])
        recomputed = path_metrics(pvals, pstamps, rec["final_equity"], capital)
        direct = rec["in_run_metrics"]
        rok = rec["reconciliation"]["ok"] and all(abs(recomputed[k] - direct[k]) <= 1e-3 for k in direct)
        cases[f"equity_reconciliation {frame}/{arm} {coin} {tf}"] = {
            "reconciliation": rec["reconciliation"], "terminal_settlement": rec["terminal_settlement"],
            "terminal_close": rec["terminal_close"], "from_persisted_path": recomputed, "from_engine_path": direct,
            "engine_total_return_pct": results["total_return_pct"], "ok": rok}
        if not rok:
            failures.append(f"equity_reconciliation {frame}/{arm} {coin} {tf}")

    for frame, inc_arm, cands, coin, tf in (("b2", "paired_incumbent", ["paired_b2_w150"], "BTC", "1h"),
                                            ("b2", "paired_incumbent", ["paired_b2_w125"], "SOL", "4h"),
                                            ("ratchet", "paired_incumbent", ["paired_r_w150"], "ETH", "30m")):
        inc, ctx = ctx_for(frame, inc_arm, coin, tf)
        sched, diag = schedule_of(inc, ctx, capital)
        fidelity, slice_parity, identity = [], [], []
        for i, s in enumerate(sched):
            r = replay(inc, ctx, 1.0, capital, s["signal_pos"], s["side"])
            fr = s["free_run"]
            fidelity.append({"entry": s["entry"], "side": s["side"], "matched": r.get("matched"),
                             "exit_equal": r.get("exit") == fr["exit"],
                             "reason_equal": r.get("exit_reason") == fr["exit_reason"],
                             "net_pct_diff": (r["net_pct"] - fr["net_pct"]) if r.get("matched") else None})
            if i < 12:
                full = replay(inc, ctx, 1.0, capital, s["signal_pos"], s["side"], lead=s["signal_pos"], horizon=None)
                keys = ("net_pct", "exit", "exit_reason", "legs", "shares")
                slice_parity.append(all(full.get(k) == r.get(k) for k in keys))
                again = replay(inc, ctx, 1.0, capital, s["signal_pos"], s["side"])
                identity.append(again.get("net_pct") == r.get("net_pct") and again.get("matched"))
        cand_rows = []
        for s in sched:
            for arm in cands:
                c = arm_candidate(frame, arm, coin, tf)
                r = replay(c, ctx, 2.0, capital, s["signal_pos"], s["side"])
                cand_rows.append({"matched": r.get("matched"), "censored": r.get("censored"),
                                  "legs": r.get("legs"), "funding": r.get("funding_usd"),
                                  "reasons": r.get("exit_reasons")})
        exits_ok = all(f["matched"] and f["exit_equal"] and f["reason_equal"] for f in fidelity)
        max_diff = max((abs(f["net_pct_diff"]) for f in fidelity if f["net_pct_diff"] is not None), default=0.0)
        ok = bool(sched) and exits_ok and all(slice_parity) and all(identity) and max_diff <= 0.05
        cases[f"replay {frame} {coin} {tf}"] = {
            "schedule": diag, "scheduled": len(sched), "replay_exit_and_reason_equal_free_run": exits_ok,
            "max_abs_net_pct_diff_vs_free_run": max_diff, "sliced_equals_full_frame": slice_parity,
            "identical_candidate_zero_delta": identity,
            "long_entries": sum(1 for s in sched if s["side"] == "long"),
            "short_entries": sum(1 for s in sched if s["side"] == "short"),
            "candidate_stress_cases": {"replays": len(cand_rows),
                                       "matched": sum(1 for r in cand_rows if r["matched"]),
                                       "partial_close_positions": sum(1 for r in cand_rows if (r["legs"] or 0) > 1),
                                       "funding_charged_positions": sum(1 for r in cand_rows if r["funding"]),
                                       "stop_exits": sum(1 for r in cand_rows if any(x == "sl" or "stop" in x
                                                                                       for x in (r["reasons"] or []))),
                                       "censored": sum(1 for r in cand_rows if r["censored"])},
            "ok": ok}
        if not ok:
            failures.append(f"replay {frame} {coin} {tf}")

    inc, ctx = ctx_for("b2", "paired_incumbent", "BTC", "1h")
    sched, _ = schedule_of(inc, ctx, capital)
    small = []
    for cap in (25.0, 5.0):
        r = replay(inc, ctx, 1.0, cap, sched[0]["signal_pos"], sched[0]["side"])
        small.append({"capital_usd": cap, "matched": r.get("matched"), "why": r.get("why"),
                      "rejected_entries": r.get("rejected_entries"), "skipped_partial_closes": r.get(
                          "skipped_partial_closes"), "legs": r.get("legs")})
    mok = small[1]["matched"] is False and small[1]["rejected_entries"] >= 1
    cases["minimum_size"] = {"replays": small, "unmatched_below_minimum": mok,
                             "partial_close_skipped_or_whole_at_25_usd": small[0]["skipped_partial_closes"] >= 1
                             or small[0]["legs"] == 1, "ok": mok}
    if not mok:
        failures.append("minimum_size")

    for coin, tf in (("BTC", "1h"), ("ETH", "4h")):
        hist, ctx = ctx_for("b2", "cont_historical", coin, tf)
        uni = arm_candidate("b2", "cont_incumbent", coin, tf)
        digests = {}
        for name, cand in (("historical", hist), ("unified", uni)):
            for cost in (1.0, 2.0):
                results, trades, _eq = run_engine(cand, ctx, cost, capital)
                digests[f"{name} x{cost:g}"] = sha256_bytes(json.dumps(
                    [[str(t.entry_date), str(t.exit_date), t.side, repr(float(t.shares)), repr(float(t.pnl)),
                      str(t.exit_reason)] for t in trades]).encode())
                digests[f"{name} x{cost:g} trades"] = len(trades)
        ok = digests["historical x1"] == digests["unified x1"] and digests["historical x2"] == digests["unified x2"]
        cases[f"b2_unified_equals_historical {coin} {tf}"] = {**digests, "ok": ok}
        if not ok:
            failures.append(f"b2_unified_equals_historical {coin} {tf}")

    tmp = tempfile.mkdtemp(prefix="mr2018_missing_", dir=work_dir)
    shutil.copytree(os.path.join(HERE, "data"), os.path.join(tmp, "data"))
    shutil.copyfile(proxy_manifest_path("1h"), os.path.join(tmp, "proxy_manifest_1h.json"))
    os.remove(os.path.join(tmp, "data", "proxy", "ETH_1h_candles.csv.gz"))
    try:
        om.load_manifest(os.path.join(tmp, "proxy_manifest_1h.json"), verify=True)
        miss = "loaded (unexpected)"
    except om.ManifestError as exc:
        miss = f"refused: {exc}"
    with open(os.path.join(tmp, "data", "proxy", "BTC_1h_candles.csv.gz"), "ab") as fh:
        fh.write(b"\0")
    try:
        om.load_manifest(os.path.join(tmp, "proxy_manifest_1h.json"), verify=True)
        changed = "loaded (unexpected)"
    except om.ManifestError as exc:
        changed = f"refused: {exc}"
    shutil.rmtree(tmp)
    iok = miss.startswith("refused") and changed.startswith("refused")
    cases["missing_or_changed_input"] = {"missing_file": miss, "changed_file": changed, "ok": iok}
    if not iok:
        failures.append("missing_or_changed_input")

    fake = {"protocol_sha256": sha256_file(PROTOCOL_PATH), "files_sha256": {}, "arms_sha256": arms_sha256(proto),
            "inputs_sha256": inputs_digest(), "code_sha256": dict(code_sha256()), "labeller_sha256": labeller_digest()}
    fake["code_sha256"]["backtest/backtester.py"] = "0" * 64
    fake["arms_sha256"]["ratchet/paired_r_w150.json"] = "0" * 64
    if os.path.exists(COVERAGE_PATH) and os.path.exists(ARM_VALIDATION_PATH) and os.path.exists(CHECKS_PATH):
        fake["files_sha256"] = locked_files(proto)
    stale = lock_mismatches(fake, proto) if os.path.exists(COVERAGE_PATH) and os.path.exists(
        ARM_VALIDATION_PATH) and os.path.exists(CHECKS_PATH) else ["not evaluated: lock inputs absent"]
    fp_a = unit_fingerprint({"phase": "x", "frame": "ratchet", "arms": ["paired_r_w150"]},
                            {"protocol_sha256": "p", "code_sha256": {}, "inputs_sha256": {},
                             "arms_sha256": {"ratchet/paired_r_w150.json": "a"}})
    fp_b = unit_fingerprint({"phase": "x", "frame": "ratchet", "arms": ["paired_r_w150"]},
                            {"protocol_sha256": "p", "code_sha256": {}, "inputs_sha256": {},
                             "arms_sha256": {"ratchet/paired_r_w150.json": "b"}})
    sok = fp_a != fp_b
    cases["stale_lock_and_cache"] = {"lock_mismatches_found": stale, "cache_fingerprint_changes_with_arm": sok,
                                     "ok": sok}
    if not sok:
        failures.append("stale_lock_and_cache")

    cases["verdict_rules"] = verdict_rule_cases(proto)
    if not cases["verdict_rules"]["ok"]:
        failures.append("verdict_rules")

    after = guarded_sha256()
    cases["isolation"] = {"guarded_unchanged": after == guarded_before, "network_calls_refused": len(NETWORK_CALLS),
                          "ok": after == guarded_before}
    if after != guarded_before:
        failures.append("isolation")
    out["failures"] = failures
    out["ok"] = not failures
    write_json(CHECKS_PATH, out)
    for k in sorted(cases):
        print(f"{k}: {'ok' if cases[k].get('ok') else 'FAIL'}")
    print(f"wrote {rel(CHECKS_PATH)}: {'ok' if out['ok'] else 'NOT ok'}")
    if failures:
        raise SystemExit(1)


def synthetic_samples(proto: dict, spec: dict) -> tuple:
    import numpy as np
    rng = np.random.default_rng(spec["seed"])
    rows = []
    start = to_ms(proto["windows"]["judge"]["start"])
    for ds, n, shift in spec["datasets"]:
        for i in range(n):
            ts = start + int(i * (500 * DAY_MS) / n)
            import pandas as pd
            base = float(rng.normal(0.0, 1.0))
            arms = {}
            for arm in ("paired_incumbent", "paired_b2_w150"):
                arms[arm] = {}
                for cost in ("base", "stress"):
                    extra = 0.0 if arm == "paired_incumbent" else shift + (spec.get("stress_shift", 0.0) if
                                                                            cost == "stress" else 0.0)
                    noise = float(rng.normal(0.0, spec["noise"])) if arm != "paired_incumbent" else 0.0
                    arms[arm][cost] = {"matched": not (spec.get("unmatched") and i % 3 == 0),
                                       "net_pct": base + extra + noise}
            rows.append({"frame": "b2", "group": "proxy", "dataset": ds, "interval": ds.split()[1],
                         "side": "long" if i % 2 else "short", "entry": str(pd.Timestamp(ts, unit="ms")),
                         "arms": arms})
    return rows


def verdict_rule_cases(proto: dict) -> dict:
    p = copy.deepcopy(proto)
    p["frames"] = {"b2": p["frames"]["b2"]}
    p["frames"]["b2"]["groups"] = {"proxy": p["frames"]["b2"]["groups"]["proxy"]}
    p["statistics"]["resamples"] = 2000
    sel = {"frames": {"b2": {"judged": ["paired_b2_w150"]}}}
    good_cont = {"pass": True, "why": ""}
    bad_cont = {"pass": False, "why": "candidate net PnL not above incumbent"}
    specs = {
        "positive": ({"seed": 1, "noise": 0.05, "datasets": [("BTC 1h", 80, 0.3), ("ETH 1h", 80, 0.3)]}, good_cont,
                     "positive"),
        "negative": ({"seed": 2, "noise": 0.05, "datasets": [("BTC 1h", 80, -0.3), ("ETH 1h", 80, -0.3)]}, good_cont,
                     "negative"),
        "harmed_dataset": ({"seed": 3, "noise": 0.05, "datasets": [("BTC 1h", 120, 0.6), ("ETH 1h", 80, -0.4)]},
                           good_cont, "negative"),
        "failed_stress": ({"seed": 4, "noise": 0.05, "stress_shift": -0.6,
                           "datasets": [("BTC 1h", 80, 0.3), ("ETH 1h", 80, 0.3)]}, good_cont, "negative"),
        "insufficient_samples": ({"seed": 5, "noise": 0.05, "datasets": [("BTC 1h", 12, 0.3)]}, good_cont,
                                 "inconclusive"),
        "low_match_coverage": ({"seed": 6, "noise": 0.05, "unmatched": True,
                                "datasets": [("BTC 1h", 80, 0.3), ("ETH 1h", 80, 0.3)]}, good_cont, "inconclusive"),
        "continuous_gate_fails": ({"seed": 7, "noise": 0.05, "datasets": [("BTC 1h", 80, 0.3), ("ETH 1h", 80, 0.3)]},
                                  bad_cont, "negative"),
    }
    out = {}
    ok = True
    global continuous_gate
    real_gate = continuous_gate
    try:
        for name, (spec, gate, want) in specs.items():
            continuous_gate = (lambda g: (lambda *_a, **_k: dict(g)))(gate)
            rows = synthetic_samples(p, spec)
            res = decide(p, rows, sel, {}, {})
            got = res["units"]["b2/proxy/both"]["verdict"]
            again = decide(p, rows, sel, {}, {})["units"]["b2/proxy/both"]["verdict"]
            out[name] = {"expected": want, "got": got, "repeat_equal": got == again}
            ok = ok and got == want and got == again
    finally:
        continuous_gate = real_gate
    try:
        judged_arms(proto, {"frames": {"b2": {"judged": ["paired_b2_w150", "paired_b2_w125", "paired_b2_w175"]}}},
                    "b2")
        expand = "accepted (unexpected)"
    except SystemExit as exc:
        expand = f"refused: {exc}"
    out["changed_shortlist_refused"] = expand
    ok = ok and expand.startswith("refused")
    return {"cases": out, "ok": ok}


def checks_post_cmd() -> None:
    import render_report
    held = require_lock(scoring=False)
    out = {"lock_sha256": sha256_file(LOCK_PATH), "measured_at_utc": now_utc(), "cases": {}}
    sel = require_selection()
    samples = read_gz_json(JUDGE_SAMPLES_PATH)
    cont = load_json(CONTINUOUS_PATH)
    equity = read_gz_json(EQUITY_PATH)
    a = decide(protocol(), samples, sel, cont, equity)
    b = decide(protocol(), samples, sel, cont, equity)
    held_verdict = load_json(VERDICT_PATH)
    same = json.dumps(a["units"], sort_keys=True, default=str) == json.dumps(b["units"], sort_keys=True, default=str)
    match = {k: held_verdict["units"][k]["verdict"] == a["units"][k]["verdict"] for k in a["units"]}
    out["cases"]["verdict_repeatable"] = {"repeat_equal": same, "matches_verdict_json": match,
                                          "ok": same and all(match.values())}
    first = render_report.render()
    second = render_report.render()
    out["cases"]["report_deterministic"] = {"equal": first == second,
                                            "matches_committed_report": os.path.exists(REPORT_PATH)
                                            and open(REPORT_PATH).read() == first,
                                            "ok": first == second}
    status = lock_commit_status()
    out["cases"]["lock_committed"] = {**status}
    recon = [r["reconciliation"]["ok"] for u in cont["units"].values() for by in u["runs"].values()
             for r in by.values()]
    out["cases"]["continuous_reconciled"] = {"runs": len(recon), "ok": all(recon)}
    log = load_json(SCORING_LOG_PATH)
    out["cases"]["scoring_isolated"] = {"events": len(log["events"]),
                                        "ok": all(e.get("guarded_unchanged") for e in log["events"])}
    out["ok"] = all(c.get("ok", True) for c in out["cases"].values())
    out["lock_protocol_sha256"] = held["protocol_sha256"]
    write_json(POST_CHECKS_PATH, out)
    for k, v in sorted(out["cases"].items()):
        print(k, "ok" if v.get("ok", True) else "FAIL")
    if not out["ok"]:
        raise SystemExit(1)


def main(argv=None) -> int:
    ap = argparse.ArgumentParser(description="issue 2018 mean_reversion exit study")
    sub = ap.add_subparsers(dest="cmd", required=True)
    sub.add_parser("copy-inputs")
    b = sub.add_parser("build-manifests")
    b.add_argument("--check", action="store_true")
    sub.add_parser("verify-inputs")
    b = sub.add_parser("build-arms")
    b.add_argument("--check", action="store_true")
    b = sub.add_parser("validate-arms")
    b.add_argument("--go-binary", required=True)
    sub.add_parser("coverage")
    b = sub.add_parser("checks")
    b.add_argument("--work-dir", required=True)
    b = sub.add_parser("lock")
    b.add_argument("--revision", default="")
    sub.add_parser("verify-lock")
    for name in ("select", "judge", "continuous"):
        b = sub.add_parser(name)
        b.add_argument("--work-dir", required=True)
        b.add_argument("--workers", type=int, default=max(1, (os.cpu_count() or 2) - 1))
    sub.add_parser("verdict")
    sub.add_parser("render")
    sub.add_parser("checks-post")
    args = ap.parse_args(argv)
    if args.cmd == "copy-inputs":
        copy_inputs()
    elif args.cmd == "build-manifests":
        build_manifests(args.check)
    elif args.cmd == "verify-inputs":
        verify_inputs()
        print("inputs ok")
    elif args.cmd == "build-arms":
        build_arms(args.check)
    elif args.cmd == "validate-arms":
        validate_arms(args.go_binary)
    elif args.cmd == "coverage":
        coverage()
    elif args.cmd == "checks":
        checks_cmd(os.path.abspath(args.work_dir))
    elif args.cmd == "lock":
        lock(args.revision)
    elif args.cmd == "verify-lock":
        held = require_lock()
        print(f"lock ok: committed in {held['_commit']['lock_commit']}, HEAD {held['_commit']['head']}")
    elif args.cmd == "select":
        select_cmd(os.path.abspath(args.work_dir), args.workers)
    elif args.cmd == "judge":
        judge_cmd(os.path.abspath(args.work_dir), args.workers)
    elif args.cmd == "continuous":
        continuous_cmd(os.path.abspath(args.work_dir), args.workers)
    elif args.cmd == "verdict":
        verdict_cmd()
    elif args.cmd == "render":
        import render_report
        render_report.main()
    elif args.cmd == "checks-post":
        checks_post_cmd()
    return 0


if __name__ == "__main__":
    if HERE not in sys.path:
        sys.path.insert(0, HERE)
    raise SystemExit(main())

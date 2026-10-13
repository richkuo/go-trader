#!/usr/bin/env python3

import argparse
import gzip
import hashlib
import io
import json
import os
import shutil
import statistics
import sys
import time
import urllib.request

import numpy as np
import pandas as pd

_HERE = os.path.dirname(os.path.abspath(__file__))
_BACKTEST = os.path.abspath(os.path.join(_HERE, "..", ".."))
_REPO = os.path.abspath(os.path.join(_BACKTEST, ".."))
sys.path.insert(0, _BACKTEST)
sys.path.insert(0, os.path.join(_REPO, "shared_tools"))

import offline_manifest as om
import trade_flow_replay as tfr
from backtester import aggregate_close_validations
from eval_windows import evaluate_window, expand_sweep, manifest_datasets
from optimizer import DEFAULT_PARAM_RANGES

MANIFEST = os.path.join(_HERE, "study_manifest.json")
SPEC_PATH = os.path.join(_HERE, "candidate_spec.json")
RECORDING_DIR = os.path.join(_HERE, "data", "recording")
SIDE_DIR = os.path.join(_HERE, "data", "side_evidence")
CANDIDATE = "cumulative_volume_delta_divergence"
BASELINE = "price_only_baseline"
SELECTION_WINDOW = "train"
HELD_OUT_WINDOW = "test"
BASE_COST = 1.0
STRESS_COST = 2.0
MIN_TRAIN_POSITIONS = 6
MIN_TEST_POSITIONS = 30
MIN_FLOW_SHARE = 0.8
MAX_COIN_PNL_SHARE = 0.6
MIN_SIDE_TAKERS = 30
COMPARISON_MODE = "approximate"
INFO_URL = "https://api.hyperliquid.xyz/info"
SOURCE_FILES = (
    "shared_strategies/open/cumulative_volume_delta_divergence.py",
    "shared_strategies/open/registry.py",
    "shared_tools/strategy_composition.py",
    "shared_tools/market_payload.py",
    "shared_strategies/close/time_stop.py",
    "backtest/backtester.py",
    "backtest/eval_windows.py",
    "backtest/offline_manifest.py",
    "backtest/trade_flow_replay.py",
    "backtest/observation_replay.py",
    "backtest/optimizer.py",
    "backtest/candidates/cumulative_volume_delta_divergence_1638/run_study.py",
    "backtest/candidates/cumulative_volume_delta_divergence_1638/side_evidence.py",
    "backtest/candidates/cumulative_volume_delta_divergence_1638/candidate_spec.json",
    "scheduler/market_feed_trades.go",
    "scheduler/market_feed_recorder.go",
    "scheduler/market_feed_ws.go",
)
FLOW_COLUMNS = ("close", "cvd_side", "cvd_setup_id", "cvd_p1_ms", "cvd_p2_ms", "cvd_confirm_ms", "cvd_p1_price",
                "cvd_p2_price", "cvd_p1_delta", "cvd_p2_delta", "cvd_delta_change", "cvd_total_volume", "cvd_ratio",
                "cvd_threshold", "cvd_reclaim", "cvd_session_ms", "cvd_flow_bars", "cvd_flow_sha256",
                "cvd_deadline_ms", "cvd_reason", "signal")


def _sha(path: str) -> str:
    return om.sha256_file(path)


def _spec() -> dict:
    with open(SPEC_PATH) as fh:
        return json.load(fh)


def _gzip_bytes(blob: bytes) -> bytes:
    buf = io.BytesIO()
    with gzip.GzipFile(filename="", mode="wb", fileobj=buf, mtime=0, compresslevel=9) as gz:
        gz.write(blob)
    return buf.getvalue()


def _raw_manifest() -> dict:
    with open(MANIFEST) as fh:
        return json.load(fh)


def _write_manifest(raw: dict) -> None:
    with open(MANIFEST, "w") as fh:
        json.dump(raw, fh, indent=2)
        fh.write("\n")


def prepare(run_dir: str) -> dict:
    loaded = tfr.load_trade_recording(run_dir)
    if os.path.exists(RECORDING_DIR):
        raise SystemExit(f"{RECORDING_DIR} exists; prepare never overwrites a frozen recording")
    os.makedirs(RECORDING_DIR)
    with open(os.path.join(run_dir, "run.manifest.json")) as fh:
        run = json.load(fh)
    shutil.copyfile(os.path.join(run_dir, "run.manifest.json"), os.path.join(RECORDING_DIR, "run.manifest.json"))
    for seg in run["segments"]:
        with open(os.path.join(run_dir, seg["file"]), "rb") as fh:
            blob = fh.read()
        with open(os.path.join(RECORDING_DIR, seg["file"] + ".gz"), "wb") as fh:
            fh.write(_gzip_bytes(blob))
        shutil.copyfile(os.path.join(run_dir, seg["file"] + ".manifest.json"),
                        os.path.join(RECORDING_DIR, seg["file"] + ".manifest.json"))
    reloaded = tfr.load_trade_recording(RECORDING_DIR)
    raw = _raw_manifest()
    out = {"run_id": loaded["report"]["run_id"], "economic_evidence": loaded["report"]["economic_evidence"],
           "refusals": loaded["report"]["refusals"], "events_compared": loaded["report"]["events_compared"],
           "series": {}}
    for ds in raw["datasets"]:
        coin = ds["coin"]
        series = reloaded["series"][coin]
        if series["intervals_sha256"] != loaded["series"][coin]["intervals_sha256"]:
            raise SystemExit(f"{coin}: the compressed copy rebuilds a different series")
        target = os.path.join(_HERE, ds["trade_flow"]["path"])
        ds["trade_flow"]["sha256"] = tfr.write_trade_series(series, target)
        out["series"][coin] = {"intervals": len(series["intervals"]), "stats": series["stats"],
                               "latency_ms": series["latency_ms"], "intervals_sha256": series["intervals_sha256"]}
    raw["recording"] = {"path": "data/recording/run.manifest.json",
                        "sha256": _sha(os.path.join(RECORDING_DIR, "run.manifest.json")),
                        "run_id": loaded["report"]["run_id"]}
    _write_manifest(raw)
    return out


def pin_side_evidence(paths: list) -> dict:
    os.makedirs(SIDE_DIR, exist_ok=True)
    raw = _raw_manifest()
    pinned = []
    for path in paths:
        name = os.path.basename(path)
        target = os.path.join(SIDE_DIR, name)
        if os.path.exists(target):
            raise SystemExit(f"{target} exists; side evidence is never overwritten")
        shutil.copyfile(path, target)
        pinned.append({"path": f"data/side_evidence/{name}", "sha256": _sha(target)})
    raw["side_evidence"] = sorted(raw.get("side_evidence", []) + pinned, key=lambda r: r["path"])
    _write_manifest(raw)
    return {"side_evidence": raw["side_evidence"]}


def _post(body: dict) -> list:
    req = urllib.request.Request(INFO_URL, data=json.dumps(body).encode("utf-8"),
                                 headers={"Content-Type": "application/json"})
    for attempt in range(5):
        try:
            with urllib.request.urlopen(req, timeout=30) as resp:
                return json.loads(resp.read())
        except Exception:
            time.sleep(5 * (attempt + 1))
    raise SystemExit(f"info request failed: {body}")


def acquire_completeness() -> dict:
    raw = _raw_manifest()
    span = raw["completeness_reference"]
    start, end = om._ts_ms(span["start"]), om._ts_ms(span["end"])
    out = {}
    for ref in span["files"]:
        coin = ref["coin"]
        rows = {}
        cursor = start
        while cursor < end:
            batch = _post({"type": "candleSnapshot", "req": {"coin": coin, "interval": "1m", "startTime": cursor,
                                                             "endTime": min(end, cursor + 4000 * 60_000)}})
            for c in batch:
                if start <= int(c["t"]) and int(c["T"]) < end:
                    rows[int(c["t"])] = {"t": int(c["t"]), "v": c["v"], "n": int(c["n"])}
            cursor += 4000 * 60_000
        target = os.path.join(_HERE, ref["path"])
        blob = json.dumps([rows[k] for k in sorted(rows)], separators=(",", ":")).encode("utf-8")
        with open(target, "wb") as fh:
            fh.write(_gzip_bytes(blob))
        ref["sha256"] = _sha(target)
        out[coin] = len(rows)
    _write_manifest(raw)
    return out


def finalize() -> dict:
    raw = _raw_manifest()
    with open(os.path.join(_HERE, raw["venue_reference"]["l2_snapshot"]["path"])) as fh:
        books = json.load(fh)
    for ds in raw["datasets"]:
        ds["half_spread_bps"] = round(float(books[ds["coin"]]["half_spread_bps"]), 4)
    _write_manifest(raw)
    return {ds["coin"]: ds["half_spread_bps"] for ds in raw["datasets"]}


def _pinned(manifest: dict, ref: dict, label: str) -> str:
    path = om._resolve(manifest["base_dir"], ref["path"])
    if not os.path.isfile(path) or _sha(path) != ref.get("sha256"):
        raise om.ManifestError(f"{label}: pinned file {ref['path']} is missing or its sha256 differs")
    return path


def verify_recording(manifest: dict) -> dict:
    rec = manifest["raw"].get("recording") or {}
    path = _pinned(manifest, rec, "recording")
    loaded = tfr.load_trade_recording(os.path.dirname(path))
    report = loaded["report"]
    out = {k: report[k] for k in ("run_id", "recorder_version", "frames", "malformed_frames", "dropped",
                                  "events_compared", "economic_evidence", "refusals", "max_frame_silence_ms",
                                  "started_ms", "closed_ms")}
    out["sessions"] = report["sessions"]
    out["segments"] = report["segments"]
    out["series"] = {}
    for ds in manifest["datasets"]:
        committed = om.load_trade_flow(ds)
        rebuilt = loaded["series"][ds["coin"]]
        if committed["intervals_sha256"] != rebuilt["intervals_sha256"]:
            raise om.ManifestError(f"{ds['key']}: the committed trade-flow series differs from the raw recording")
        reasons = {}
        for iv in rebuilt["intervals"]:
            if iv["status"] == "invalid" or iv["invalid_ms"] is not None:
                reasons[iv["reason"]] = reasons.get(iv["reason"], 0) + 1
        out["series"][ds["coin"]] = {"intervals": len(rebuilt["intervals"]), "stats": rebuilt["stats"],
                                     "latency_ms": rebuilt["latency_ms"],
                                     "intervals_sha256": rebuilt["intervals_sha256"],
                                     "invalid_reasons": dict(sorted(reasons.items()))}
    return out


def verify_side_evidence(manifest: dict) -> dict:
    refs = manifest["raw"].get("side_evidence") or []
    files = []
    takers = agree = disagree = mismatch = sampled = 0
    for ref in refs:
        path = _pinned(manifest, ref, "side_evidence")
        with open(path) as fh:
            doc = json.load(fh)
        rows = doc["trades"]
        f_takers = [r for r in rows if r["facts"]["taker_side"] is not None]
        f_agree = sum(1 for r in f_takers if r["facts"]["taker_side"] == r["trade"]["side"])
        f_mismatch = sum(1 for r in rows if not r["facts"]["fill_matches_trade"])
        files.append({"path": ref["path"], "segment": doc["segment"], "collected_at_ms": doc["collected_at_ms"],
                      "sampled": len(rows), "taker_identified": len(f_takers), "side_equals_taker": f_agree,
                      "fill_content_mismatch": f_mismatch, "evidence_version": doc["evidence_version"]})
        sampled += len(rows)
        takers += len(f_takers)
        agree += f_agree
        disagree += len(f_takers) - f_agree
        mismatch += f_mismatch
    verified = takers >= MIN_SIDE_TAKERS and disagree == 0 and mismatch == 0
    return {"files": files, "sampled": sampled, "taker_identified": takers, "side_equals_taker": agree,
            "side_differs_from_taker": disagree, "fill_content_mismatch": mismatch, "verified": verified,
            "rule": f"verified when at least {MIN_SIDE_TAKERS} trades identify a taker, every taker agrees with the side "
                    "field and every matched fill carries the trade's px, sz, time and hash"}


def measure_completeness(manifest: dict) -> dict:
    span = manifest["raw"]["completeness_reference"]
    out = {}
    for ref in span["files"]:
        path = _pinned(manifest, ref, "completeness_reference")
        with gzip.open(path, "rb") as fh:
            candles = {c["t"]: c for c in json.loads(fh.read())}
        ds = next(d for d in manifest["datasets"] if d["coin"] == ref["coin"])
        series = om.load_trade_flow(ds)
        complete = [iv for iv in series["intervals"] if iv["status"] == "complete" and iv["invalid_ms"] is None]
        both = vol_only = tally_only = neither = missing = 0
        examples = []
        for iv in complete:
            c = candles.get(iv["start_ms"])
            if c is None:
                missing += 1
                continue
            v_ok = tfr.canonical_decimal(c["v"]) == iv["total"]
            n_ok = int(c["n"]) == int(iv["trades"])
            if v_ok and n_ok:
                both += 1
            elif v_ok:
                vol_only += 1
            elif n_ok:
                tally_only += 1
            else:
                neither += 1
            if not (v_ok and n_ok) and len(examples) < 10:
                examples.append({"start_ms": iv["start_ms"], "recorded_total": iv["total"], "recorded_trades": iv["trades"],
                                 "candle_v": c["v"], "candle_n": c["n"]})
        compared = both + vol_only + tally_only + neither
        out[ref["coin"]] = {"complete_intervals": len(complete), "compared": compared, "missing_candle": missing,
                            "volume_and_tally_match": both, "volume_only_match": vol_only, "tally_only_match": tally_only,
                            "neither_match": neither,
                            "match_share": round(both / compared, 6) if compared else None, "mismatch_examples": examples}
    return out


def funding_sign_check() -> dict:
    from backtester import Backtester
    n = 30
    idx = pd.date_range("2026-01-01", periods=n, freq="5min")
    frame = pd.DataFrame({"open": 100.0, "high": 100.5, "low": 99.5, "close": 100.0, "volume": 1.0}, index=idx)
    frame["timestamp"] = (idx.asi8 // 1_000_000).astype("int64")
    frame["funding_accrual"] = 0.0
    frame.iloc[10, frame.columns.get_loc("funding_accrual")] = 0.0001
    frame["atr"] = 1.0
    out = {}
    for side, sig in (("long", 1), ("short", -1)):
        df = frame.copy()
        df["signal"] = 0
        df.iloc[2, df.columns.get_loc("signal")] = sig
        bt = Backtester(initial_capital=1000.0, platform="hyperliquid", direction="both",
                        close_strategies=[{"name": "time_stop", "params": {"max_bars": 20}}],
                        open_strategy={"name": "synthetic", "params": {}}, comparison_mode=COMPARISON_MODE)
        res = bt.run(df, strategy_name="synthetic", symbol="BTC/USDT", timeframe="5m", save=False)
        out[side] = round(float(res.get("total_funding_pnl") or (res.get("metrics") or {}).get("total_funding_pnl") or
                                sum(t.get("funding_pnl", 0.0) for t in res["trades"])), 6)
    out["long_pays_positive_rate"] = out["long"] < 0
    out["short_receives_positive_rate"] = out["short"] > 0
    return out


def arm(spec: dict, name: str, params: dict) -> dict:
    ex = spec["exit_and_risk"]
    return {"name": name, "params": dict(params), "direction": ex["direction"],
            "close_strategies": [dict(ex["close_strategy"], params=dict(ex["close_strategy"]["params"]))],
            "stop_loss_atr_mult": 1.0, "comparison_mode": COMPARISON_MODE}


def _grid(defaults: dict) -> list:
    specs = [(k, list(v)) for k, v in DEFAULT_PARAM_RANGES[CANDIDATE].items()]
    return expand_sweep(dict(defaults), specs)


def candidate_frame(reg, manifest, dataset, window_name, params):
    frame, win, _ = om.window_frame(manifest, dataset, window_name)
    flow, _ = om.attach_trade_flow(manifest, dataset, win)
    out = reg.apply_strategy(CANDIDATE, frame, {**params, "trade_flow_observations": flow})
    return frame, win, out


def flow_share(reg, manifest, window_name, params) -> dict:
    out = {}
    for ds in manifest["datasets"]:
        _, win, cand = candidate_frame(reg, manifest, ds, window_name, params)
        scored = om.slice_window(cand, win)
        out[ds["key"]] = {"bars": int(len(scored)), "flow_available_bars": int(scored["cvd_flow_available"].sum()),
                          "share": round(float(scored["cvd_flow_available"].mean()), 4) if len(scored) else 0.0,
                          "reasons": {str(k): int(v) for k, v in scored["cvd_reason"].value_counts().items()}}
    return out


def _clean(v):
    if hasattr(v, "item"):
        v = v.item()
    if isinstance(v, float) and v != v:
        return None
    return v


def entry_diagnostics(reg, manifest, window_name, params) -> list:
    rows = []
    for ds in manifest["datasets"]:
        _, win, cand = candidate_frame(reg, manifest, ds, window_name, params)
        scored = om.slice_window(cand, win)
        mask = scored["cvd_reason"].isin(["entry_long", "entry_short", "armed_long", "armed_short"])
        for ts, r in scored[mask][list(FLOW_COLUMNS)].iterrows():
            entry = {"dataset": ds["key"], "bar_open": str(ts)}
            for c in FLOW_COLUMNS:
                entry[c] = _clean(r[c])
            rows.append(entry)
    return rows


def summarize(legs: dict) -> dict:
    present = {k: v for k, v in legs.items() if v is not None}
    if not present:
        return {"datasets": 0, "positions": 0, "close_validation": aggregate_close_validations([])}
    positions = []
    per = {}
    for key, leg in sorted(present.items()):
        ex = leg["execution"]
        per[key] = {"return_pct": leg["return_pct"], "sharpe": leg["sharpe"], "max_dd_pct": leg["max_dd_pct"],
                    "ddadj": leg["ddadj"], "net_pnl_usd": ex["net_pnl_usd"], "fees_usd": ex["fees_usd"],
                    "funding_pnl_usd": ex["funding_pnl_usd"], "turnover": ex["turnover"],
                    "positions": ex["positions"], "long_positions": ex["long_positions"],
                    "short_positions": ex["short_positions"]}
        positions += [dict(p, dataset=key) for p in ex["position_list"]]
    abs_ds = [abs(v["net_pnl_usd"]) for v in per.values()]
    spans = [(pd.Timestamp(p["entry_date"]), pd.Timestamp(p["exit_date"]), p["side"], p["dataset"]) for p in positions]
    overlap = sum(1 for i, (s, e, side, ds) in enumerate(spans)
                  if any(ds2 != ds and side2 == side and s2 <= e and s <= e2
                         for j, (s2, e2, side2, ds2) in enumerate(spans) if j != i))
    return {
        "datasets": len(per),
        "mean_return_pct": round(statistics.mean(v["return_pct"] for v in per.values()), 4),
        "mean_sharpe": round(statistics.mean(v["sharpe"] for v in per.values()), 4),
        "mean_ddadj": round(statistics.mean(v["ddadj"] for v in per.values()), 4),
        "worst_max_dd_pct": round(min(v["max_dd_pct"] for v in per.values()), 4),
        "net_pnl_usd": round(sum(v["net_pnl_usd"] for v in per.values()), 4),
        "fees_usd": round(sum(v["fees_usd"] for v in per.values()), 4),
        "funding_pnl_usd": round(sum(v["funding_pnl_usd"] for v in per.values()), 4),
        "mean_turnover": round(statistics.mean(v["turnover"] for v in per.values()), 4),
        "positions": len(positions),
        "long_positions": sum(1 for p in positions if p["side"] == "long"),
        "short_positions": sum(1 for p in positions if p["side"] == "short"),
        "top_dataset_abs_pnl_share": round(max(abs_ds) / sum(abs_ds), 4) if sum(abs_ds) > 0 else None,
        "same_side_cross_asset_overlap_share": round(overlap / len(positions), 4) if positions else None,
        "per_dataset": per,
        "close_validation": aggregate_close_validations(leg.get("close_validation") for leg in present.values()),
    }


def _legs(score: dict) -> dict:
    return {r["dataset"]: r["leg"] for r in score["rows"]}


def select(rows: list, n_datasets: int) -> dict:
    eligible = [(i, r) for i, r in enumerate(rows)
                if r["summary"]["datasets"] == n_datasets and r["summary"]["positions"] >= MIN_TRAIN_POSITIONS]
    if not eligible:
        return {"rule_outcome": "no_eligible_combo", "selected": None}
    best = max(eligible, key=lambda kv: (kv[1]["summary"]["mean_ddadj"], kv[1]["summary"]["mean_sharpe"], -kv[0]))[1]
    return {"rule_outcome": "selected", "selected": best["label"], "params": best["params"]}


def verdict(cand_base, cand_stress, base, shares, side, recording, candle_cov) -> dict:
    checks = {
        "side_evidence_verified": bool(side["verified"]),
        "recording_is_economic_evidence": bool(recording["economic_evidence"]),
        "sample_sufficient": cand_base["positions"] >= MIN_TEST_POSITIONS,
        "flow_coverage": all(v["share"] >= MIN_FLOW_SHARE for v in shares.values()),
        "candle_coverage_complete": candle_cov,
        "net_positive_base": cand_base.get("net_pnl_usd", 0) > 0,
        "net_positive_stress": cand_stress.get("net_pnl_usd", 0) > 0,
        "beats_price_only_ddadj": cand_base.get("mean_ddadj", 0) > base.get("mean_ddadj", 0),
        "not_concentrated": (cand_base.get("top_dataset_abs_pnl_share") or 1.0) <= MAX_COIN_PNL_SHARE,
    }
    gates = ("side_evidence_verified", "recording_is_economic_evidence", "sample_sufficient", "flow_coverage",
             "candle_coverage_complete")
    missing = []
    if not checks["side_evidence_verified"]:
        missing.append("side evidence is not verified")
    if not checks["recording_is_economic_evidence"]:
        missing.append("the recording is incomplete or damaged: " + "; ".join(recording["refusals"]))
    if not checks["sample_sufficient"]:
        missing.append(f"{cand_base['positions']} held-out positions < {MIN_TEST_POSITIONS}")
    if not checks["flow_coverage"]:
        low = sorted(k for k, v in shares.items() if v["share"] < MIN_FLOW_SHARE)
        missing.append(f"usable-flow bar share below {MIN_FLOW_SHARE} on {', '.join(low)}")
    if not checks["candle_coverage_complete"]:
        missing.append("held-out candle coverage is incomplete")
    if not all(checks[g] for g in gates):
        outcome, reason = "inconclusive", "; ".join(missing)
    elif all(checks.values()):
        outcome, reason = "pass", "every frozen held-out check passed"
    else:
        outcome, reason = "fail", "failed frozen checks: " + ", ".join(sorted(k for k, v in checks.items() if not v))
    return {"outcome": outcome, "reason": reason, "checks": checks,
            "live_status": "production_refused regardless of outcome; promotion needs a separate reviewed decision"}


def run(manifest_path: str) -> dict:
    from registry_loader import load_registry
    spec = _spec()
    manifest = om.load_manifest(manifest_path)
    recording = verify_recording(manifest)
    side = verify_side_evidence(manifest)
    completeness = measure_completeness(manifest)
    funding = funding_sign_check()
    reg = load_registry("futures")
    datasets = manifest_datasets(manifest)
    memo: dict = {}

    def score(params, window, cost):
        return evaluate_window(reg, arm(spec, CANDIDATE, params), datasets, window,
                               spec["exit_and_risk"]["capital_usd"], memo, manifest=manifest, cost_multiplier=cost)

    defaults = dict(reg.STRATEGY_REGISTRY[CANDIDATE]["default_params"])
    train_rows = []
    for label, params in _grid(defaults):
        summary = summarize(_legs(score(params, SELECTION_WINDOW, BASE_COST)))
        summary.pop("per_dataset", None)
        train_rows.append({"label": label, "params": params, "summary": summary})
    choice = select(train_rows, len(datasets))
    selected = choice["params"] if choice["selected"] else defaults
    baseline_params = dict(selected, delta_confirmation=False)

    held = {}
    cand_score = score(selected, HELD_OUT_WINDOW, BASE_COST)
    held[CANDIDATE] = {"params": selected, "base": summarize(_legs(cand_score)),
                       "stress": summarize(_legs(score(selected, HELD_OUT_WINDOW, STRESS_COST))),
                       "m1_incumbent_verdict": cand_score.get("verdict"),
                       "m1_incumbent_bar_mean_ddadj": cand_score.get("mean_bar_ddadj")}
    held[BASELINE] = {"params": baseline_params,
                      "base": summarize(_legs(score(baseline_params, HELD_OUT_WINDOW, BASE_COST))),
                      "stress": summarize(_legs(score(baseline_params, HELD_OUT_WINDOW, STRESS_COST)))}
    train_held = {CANDIDATE: summarize(_legs(score(selected, SELECTION_WINDOW, BASE_COST))),
                  BASELINE: summarize(_legs(score(baseline_params, SELECTION_WINDOW, BASE_COST)))}
    shares = {w: flow_share(reg, manifest, w, selected) for w in (SELECTION_WINDOW, HELD_OUT_WINDOW)}
    candle_cov = True
    window_coverage = {}
    for ds in manifest["datasets"]:
        window_coverage[ds["key"]] = {}
        for w in (SELECTION_WINDOW, HELD_OUT_WINDOW):
            _, win, cov = om.window_frame(manifest, ds, w)
            window_coverage[ds["key"]][w] = {"candles": cov,
                                             "trade_flow": om.trade_flow_coverage(om.load_trade_flow(ds), win["start_ms"], win["end_ms"])}
            if w == HELD_OUT_WINDOW and not cov.get("complete", True):
                candle_cov = False

    variants = []
    positive_train = 0
    for row in train_rows:
        test_summary = summarize(_legs(score(row["params"], HELD_OUT_WINDOW, BASE_COST)))
        if (row["summary"].get("net_pnl_usd") or 0) > 0:
            positive_train += 1
        variants.append({"label": row["label"], "params": row["params"],
                         "train_positions": row["summary"]["positions"],
                         "train_net_pnl_usd": row["summary"].get("net_pnl_usd"),
                         "train_mean_ddadj": row["summary"].get("mean_ddadj"),
                         "test_positions": test_summary["positions"],
                         "test_mean_ddadj": test_summary.get("mean_ddadj"),
                         "test_net_pnl_usd": test_summary.get("net_pnl_usd")})

    result = {
        "study": manifest["study"],
        "issue": 1638,
        "spec_sha256": _sha(SPEC_PATH),
        "manifest": {
            "path": os.path.relpath(manifest["path"], _REPO),
            "sha256": _sha(manifest["path"]),
            "schema": manifest["schema"],
            "provenance": manifest["provenance"],
            "interval": manifest["interval"],
            "warmup_bars": manifest["warmup_bars"],
            "windows": {k: {"start": v["start"], "end": v["end"], "role": v["role"]} for k, v in manifest["windows"].items()},
            "costs": manifest["costs"],
            "datasets": [{"key": d["key"], "size_decimals": d["size_decimals"], "half_spread_bps": d["half_spread_bps"],
                          "candles_sha256": d["candles"]["sha256"],
                          "funding_sha256": d["funding"]["sha256"] if d["funding"] else None,
                          "trade_flow_sha256": d["trade_flow"]["sha256"]} for d in manifest["datasets"]],
            "venue_reference": {"meta_sha256": manifest["venue_reference"]["meta"]["sha256"],
                                "l2_snapshot_sha256": manifest["venue_reference"].get("l2_snapshot", {}).get("sha256"),
                                "documents": manifest["venue_reference"]["documents"]},
            "recording": manifest["raw"].get("recording"),
            "side_evidence": manifest["raw"].get("side_evidence"),
            "completeness_reference": manifest["raw"].get("completeness_reference"),
        },
        "recording": recording,
        "side_evidence": side,
        "completeness": completeness,
        "funding_sign_check": funding,
        "window_coverage": window_coverage,
        "source_sha256": {p: _sha(os.path.join(_REPO, p)) for p in SOURCE_FILES},
        "protocol": {
            "close_strategy": spec["exit_and_risk"]["close_strategy"],
            "comparison_mode": COMPARISON_MODE,
            "stop": spec["exit_and_risk"]["stop_owner"],
            "direction": spec["exit_and_risk"]["direction"],
            "capital_usd": spec["exit_and_risk"]["capital_usd"],
            "sizing": spec["exit_and_risk"]["sizing"],
            "selection_rule": spec["selection_rule"],
            "min_train_positions": MIN_TRAIN_POSITIONS,
            "verdict_criteria": spec["verdict_criteria"],
            "cost_multipliers": {"base": BASE_COST, "stress": STRESS_COST},
        },
        "selection": {"grid": {k: list(v) for k, v in DEFAULT_PARAM_RANGES[CANDIDATE].items()},
                      "train_sweep": train_rows, "choice": choice, "selected_params": selected,
                      "stability": {"grid_points": len(train_rows), "positive_train_net": positive_train,
                                    "positive_share": round(positive_train / len(train_rows), 4) if train_rows else None}},
        "flow_share": shares,
        "held_out": held,
        "train_selected": train_held,
        "held_out_events": entry_diagnostics(reg, manifest, HELD_OUT_WINDOW, selected),
        "train_events": entry_diagnostics(reg, manifest, SELECTION_WINDOW, selected),
        "variants": variants,
    }
    result["verdict"] = verdict(held[CANDIDATE]["base"], held[CANDIDATE]["stress"], held[BASELINE]["base"],
                                shares[HELD_OUT_WINDOW], side, recording, candle_cov)
    result["close_validation"] = aggregate_close_validations(
        held[a][k].get("close_validation") for a in held for k in ("base", "stress"))
    return result


def _fmt(v, nd=2):
    if v is None:
        return "-"
    if isinstance(v, bool):
        return "yes" if v else "no"
    if isinstance(v, float):
        return f"{v:.{nd}f}"
    return str(v)


def _ts(ms):
    return "-" if ms is None else str(pd.Timestamp(int(ms), unit="ms", tz="UTC")).replace("+00:00", "Z")


def render(result: dict) -> str:
    v = result["verdict"]
    m = result["manifest"]
    r = result["recording"]
    s = result["side_evidence"]
    lines = [
        "# Cumulative volume delta divergence: recorded held-out result (#1638)",
        "",
        f"Verdict: **{v['outcome'].upper()}**. {v['reason']}. {v['live_status']}.",
        "",
        "Generated by `run_study.py` from `results.json`; do not edit by hand.",
        "",
        "## Inputs",
        "",
        f"- Manifest `{m['path']}` ({m['schema']}) sha256 `{m['sha256']}`",
        f"- Specification `candidate_spec.json` sha256 `{result['spec_sha256']}`",
        f"- Provenance: {m['provenance']['label']}",
        f"- Interval {m['interval']}, warm-up {m['warmup_bars']} bars (not scored)",
    ]
    for name, w in sorted(m["windows"].items(), key=lambda kv: kv[1]["start"]):
        lines.append(f"- Window `{name}` [{w['start']}, {w['end']}): {w['role']}")
    c = m["costs"]
    lines.append(f"- Costs: taker {c['taker_fee_pct']*100:.3f}%, maker {c['maker_fee_pct']*100:.3f}%, slippage "
                 f"{c['slippage_bps']} bps per side plus per-coin half spread, minimum order ${c['min_notional_usd']:.0f} "
                 f"with a {c['min_notional_margin']*100:.0f}% margin, funding booked hourly for every arm")
    for d in m["datasets"]:
        lines.append(f"- `{d['key']}`: szDecimals {d['size_decimals']}, half spread {d['half_spread_bps']} bps, candles "
                     f"`{d['candles_sha256'][:16]}`, funding `{(d['funding_sha256'] or '')[:16]}`, trade flow "
                     f"`{d['trade_flow_sha256'][:16]}`")
    f = result["funding_sign_check"]
    lines += [
        f"- Funding sign check (synthetic +0.01% accrual, real Backtester): long {f['long']}, short {f['short']} "
        f"(long pays: {_fmt(f['long_pays_positive_rate'])}, short receives: {_fmt(f['short_receives_positive_rate'])})",
        "",
        "## Recording and replay",
        "",
        f"- Run `{r['run_id']}` (recorder `{r['recorder_version']}`), {_ts(r['started_ms'])} to {_ts(r['closed_ms'])}: "
        f"{len(r['segments'])} hashed segments, {r['frames']} trade frames, {r['malformed_frames']} malformed frames, "
        f"{r['dropped']} dropped records, sessions {r['sessions']}.",
        f"- Independent Python replay reproduced every per-trade outcome and all {r['events_compared']} recorder "
        f"interval, invalidation and coverage events. Economic evidence: {_fmt(r['economic_evidence'])}"
        + (f" ({'; '.join(r['refusals'])})" if r["refusals"] else "") + ".",
        "",
        "| Coin | Intervals | Accepted | Duplicates | Conflicts | Late | Uncovered | Rejected | Invalid | Latency p50/p99/max ms | Max frame silence ms | Invalid reasons |",
        "|---|---|---|---|---|---|---|---|---|---|---|---|",
    ]
    for coin, sr in sorted(r["series"].items()):
        st, lat = sr["stats"], sr["latency_ms"]
        reasons = ", ".join(f"{k} {n}" for k, n in sr["invalid_reasons"].items()) or "-"
        lines.append(f"| {coin} | {sr['intervals']} | {st['accepted']} | {st['duplicates']} | {st['conflicts']} | "
                     f"{st['late']} | {st['uncovered']} | {st['rejected']} | {st['invalid']} | "
                     f"{lat['p50']}/{lat['p99']}/{lat['max']} | {r['max_frame_silence_ms'].get(coin)} | {reasons} |")
    lines += ["", "## Completeness against the venue 1m candles", "",
              "| Coin | Complete intervals | Compared | Volume and tally match | Volume only | Tally only | Neither | Match share |",
              "|---|---|---|---|---|---|---|---|"]
    for coin, cm in sorted(result["completeness"].items()):
        lines.append(f"| {coin} | {cm['complete_intervals']} | {cm['compared']} | {cm['volume_and_tally_match']} | "
                     f"{cm['volume_only_match']} | {cm['tally_only_match']} | {cm['neither_match']} | {_fmt(cm['match_share'], 4)} |")
    lines += ["", "## Side evidence", "",
              f"- {s['sampled']} recorded trades sampled; {s['taker_identified']} identified a taker through public "
              f"userFillsByTime crossed flags; side equals taker in {s['side_equals_taker']}, differs in "
              f"{s['side_differs_from_taker']}; fill content mismatches {s['fill_content_mismatch']}. Verified: "
              f"{_fmt(s['verified'])}. Rule: {s['rule']}."]
    for fe in s["files"]:
        lines.append(f"- `{fe['path']}` from `{fe['segment']}` at {_ts(fe['collected_at_ms'])}: {fe['sampled']} sampled, "
                     f"{fe['taker_identified']} takers, {fe['side_equals_taker']} agree")
    p = result["protocol"]
    lines += [
        "",
        "## Protocol",
        "",
        f"- Every arm: direction `{p['direction']}`, close `time_stop` (max_bars {p['close_strategy']['params']['max_bars']}), "
        f"{p['stop']}, capital ${p['capital_usd']:.0f}; {p['sizing']}.",
        f"- Close comparison mode `{p['comparison_mode']}`: time_stop reads the simulator's held-bar count, which no live "
        "close context supplies, so this is research evidence with incomplete close parity.",
        "- Baseline `price_only_baseline`: the same evaluator, parameters and flow-usability rules with "
        "delta_confirmation=false, so both arms score the same complete-data bars.",
        f"- Selection: {p['selection_rule']}",
        f"- Selection outcome: {result['selection']['choice']['rule_outcome']}; scored params "
        f"`{json.dumps(result['selection']['selected_params'], sort_keys=True)}`",
        f"- Parameter stability: {result['selection']['stability']['positive_train_net']} of "
        f"{result['selection']['stability']['grid_points']} grid points had positive train net PnL.",
        "",
        "## Usable-flow share (selected params)",
        "",
        "| Window | Dataset | Scored bars | Flow-usable bars | Share | Reasons |",
        "|---|---|---|---|---|---|",
    ]
    for w, per in result["flow_share"].items():
        for ds, sh in sorted(per.items()):
            reasons = ", ".join(f"{k} {n}" for k, n in sorted(sh["reasons"].items()))
            lines.append(f"| {w} | {ds} | {sh['bars']} | {sh['flow_available_bars']} | {_fmt(sh['share'], 3)} | {reasons} |")
    lines += ["", "## Held-out comparison", "",
              "| Arm | Cost | Positions (L/S) | Net $ | Fees $ | Funding $ | Mean ret % | Mean DDadj | Worst DD % | Turnover | Top coin share | Overlap share |",
              "|---|---|---|---|---|---|---|---|---|---|---|---|"]
    for name, h in result["held_out"].items():
        for label in ("base", "stress"):
            sm = h[label]
            if not sm.get("datasets"):
                lines.append(f"| `{name}` | {label} | 0 | - | - | - | - | - | - | - | - | - |")
                continue
            lines.append(f"| `{name}` | {label} | {sm['positions']} ({sm['long_positions']}/{sm['short_positions']}) | "
                         f"{_fmt(sm['net_pnl_usd'])} | {_fmt(sm['fees_usd'])} | {_fmt(sm['funding_pnl_usd'], 4)} | "
                         f"{_fmt(sm['mean_return_pct'], 3)} | {_fmt(sm['mean_ddadj'], 3)} | {_fmt(sm['worst_max_dd_pct'], 3)} | "
                         f"{_fmt(sm['mean_turnover'])} | {_fmt(sm['top_dataset_abs_pnl_share'])} | "
                         f"{_fmt(sm['same_side_cross_asset_overlap_share'])} |")
    lines += ["", "## Train window (selected params)", "",
              "| Arm | Positions (L/S) | Net $ | Mean DDadj |", "|---|---|---|---|"]
    for name, sm in result["train_selected"].items():
        if not sm.get("datasets"):
            lines.append(f"| `{name}` | 0 | - | - |")
            continue
        lines.append(f"| `{name}` | {sm['positions']} ({sm['long_positions']}/{sm['short_positions']}) | "
                     f"{_fmt(sm['net_pnl_usd'])} | {_fmt(sm['mean_ddadj'], 3)} |")
    for title, key in (("Held-out armed and entered setups", "held_out_events"), ("Train armed and entered setups", "train_events")):
        lines += ["", f"## {title} (candidate, selected params)", ""]
        events = result[key]
        if not events:
            lines.append("None.")
            continue
        lines += ["| Dataset | Bar open | Event | Pivot 1 -> 2 | Prices | CVD 1 -> 2 | Ratio | Total vol | Reclaim | Close | Flow sha |",
                  "|---|---|---|---|---|---|---|---|---|---|---|"]
        for e in events:
            lines.append(f"| {e['dataset']} | {e['bar_open']} | {e['cvd_reason']} | {_ts(e['cvd_p1_ms'])} -> {_ts(e['cvd_p2_ms'])} | "
                         f"{_fmt(e['cvd_p1_price'], 2)} -> {_fmt(e['cvd_p2_price'], 2)} | {_fmt(e['cvd_p1_delta'], 4)} -> "
                         f"{_fmt(e['cvd_p2_delta'], 4)} | {_fmt(e['cvd_ratio'], 4)} | {_fmt(e['cvd_total_volume'], 4)} | "
                         f"{_fmt(e['cvd_reclaim'], 2)} | {_fmt(e['close'], 2)} | `{(e['cvd_flow_sha256'] or '')[:12]}` |")
    lines += ["", "## Verdict checks", ""]
    for k, ok in sorted(v["checks"].items()):
        lines.append(f"- {k}: {_fmt(ok)}")
    lines += ["", "## All grid variants (train selection, held-out outcome)", "",
              "| Variant | Train positions | Train net $ | Train DDadj | Held-out positions | Held-out DDadj | Held-out net $ |",
              "|---|---|---|---|---|---|---|"]
    for row in result["variants"]:
        lines.append(f"| {row['label']} | {row['train_positions']} | {_fmt(row['train_net_pnl_usd'])} | "
                     f"{_fmt(row['train_mean_ddadj'], 3)} | {row['test_positions']} | {_fmt(row['test_mean_ddadj'], 3)} | "
                     f"{_fmt(row['test_net_pnl_usd'])} |")
    lines += ["", "## Source hashes", ""]
    for path, digest in sorted(result["source_sha256"].items()):
        lines.append(f"- `{path}` `{digest}`")
    lines.append("")
    return "\n".join(lines)


def main(argv=None) -> int:
    p = argparse.ArgumentParser(description="#1638 recorded cumulative volume delta study driver (research-only)")
    sub = p.add_subparsers(dest="cmd")
    prep = sub.add_parser("prepare", help="Freeze a closed trade recording into data/ and pin its series hashes")
    prep.add_argument("--run-dir", required=True)
    side = sub.add_parser("pin-side-evidence", help="Copy side_evidence.py outputs into data/ and pin their hashes")
    side.add_argument("paths", nargs="+")
    sub.add_parser("acquire-completeness", help="Fetch the venue 1m candles for the recording span and pin them")
    sub.add_parser("finalize", help="Pin half spreads from the acquired l2 snapshot")
    runp = sub.add_parser("run", help="Score the frozen study (default)")
    runp.add_argument("--manifest", default=MANIFEST)
    runp.add_argument("--render-only", action="store_true")
    args = p.parse_args(argv)
    json_out = os.path.join(_HERE, "results.json")
    report = os.path.join(_HERE, "REPORT.md")
    try:
        if args.cmd == "prepare":
            print(json.dumps(prepare(args.run_dir), indent=2, sort_keys=True))
            return 0
        if args.cmd == "pin-side-evidence":
            print(json.dumps(pin_side_evidence(args.paths), indent=2, sort_keys=True))
            return 0
        if args.cmd == "acquire-completeness":
            print(json.dumps(acquire_completeness(), indent=2, sort_keys=True))
            return 0
        if args.cmd == "finalize":
            print(json.dumps(finalize(), indent=2, sort_keys=True))
            return 0
        if getattr(args, "render_only", False):
            with open(json_out) as fh:
                result = json.load(fh)
        else:
            result = run(getattr(args, "manifest", MANIFEST))
            with open(json_out, "w") as fh:
                json.dump(result, fh, indent=1, sort_keys=True, default=str)
                fh.write("\n")
        with open(report, "w") as fh:
            fh.write(render(result))
        print(json.dumps({"verdict": result["verdict"]}, indent=2, sort_keys=True))
        return 0
    except (om.ManifestError, tfr.TradeRecordingError) as exc:
        print(f"study error: {exc}", file=sys.stderr)
        return 1


if __name__ == "__main__":
    sys.exit(main())

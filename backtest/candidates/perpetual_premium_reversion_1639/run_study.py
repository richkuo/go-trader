#!/usr/bin/env python3

import argparse
import gzip
import io
import json
import os
import shutil
import statistics
import sys

import numpy as np
import pandas as pd

_HERE = os.path.dirname(os.path.abspath(__file__))
_BACKTEST = os.path.abspath(os.path.join(_HERE, "..", ".."))
_REPO = os.path.abspath(os.path.join(_BACKTEST, ".."))
sys.path.insert(0, _BACKTEST)
sys.path.insert(0, os.path.join(_REPO, "shared_tools"))
sys.path.insert(0, _HERE)

import event_replay as er
import offline_manifest as om
import premium_replay as pr
from atr import ensure_atr_indicator
from backtester import aggregate_close_validations
from eval_windows import evaluate_window, expand_sweep, manifest_datasets
from optimizer import DEFAULT_PARAM_RANGES

MANIFEST = os.path.join(_HERE, "study_manifest.json")
SPEC_PATH = os.path.join(_HERE, "candidate_spec.json")
RECORDING_DIR = os.path.join(_HERE, "data", "recording")
CANDIDATE = "perpetual_premium_reversion"
BASELINES = {"mean_reversion_pro": {}, "funding_skew": {"funding_window": 360}}
SELECTION_WINDOW = "train"
HELD_OUT_WINDOW = "test"
BASE_COST = 1.0
STRESS_COST = 2.0
MIN_TRAIN_POSITIONS = 10
MIN_TEST_POSITIONS = 30
MIN_VALID_SHARE = 0.7
MAX_COIN_PNL_SHARE = 0.6
COMPARISON_MODE = "approximate"
DATA_VALID_REASONS = frozenset({"no_extreme", "no_turn", "wrong_premium_sign", "entry_long", "entry_short", "variance_floor"})
SOURCE_FILES = (
    "shared_strategies/open/perpetual_premium_reversion.py",
    "shared_strategies/open/registry.py",
    "shared_strategies/open/funding_skew.py",
    "shared_tools/strategy_composition.py",
    "shared_tools/market_payload.py",
    "shared_strategies/close/time_stop.py",
    "backtest/backtester.py",
    "backtest/eval_windows.py",
    "backtest/offline_manifest.py",
    "backtest/premium_replay.py",
    "backtest/observation_kinds.py",
    "backtest/observation_replay.py",
    "backtest/optimizer.py",
    "backtest/candidates/perpetual_premium_reversion_1639/run_study.py",
    "backtest/candidates/perpetual_premium_reversion_1639/event_replay.py",
    "backtest/candidates/perpetual_premium_reversion_1639/candidate_spec.json",
    "scheduler/market_feed_premium.go",
    "scheduler/market_feed_recorder.go",
    "scheduler/market_feed_ws.go",
)
EVIDENCE_COLUMNS = ("close", "ppr_sample_ms", "ppr_bid", "ppr_ask", "ppr_oracle", "ppr_mark", "ppr_spread_bps",
                    "ppr_quote_event_ms", "ppr_quote_recv_ms", "ppr_context_recv_ms", "ppr_session", "ppr_premium",
                    "ppr_extreme_premium", "ppr_mean", "ppr_std", "ppr_z_extreme", "ppr_z_current", "ppr_side",
                    "ppr_window_sha256", "ppr_deadline_ms", "ppr_reason", "signal")


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


DOC_SOURCE = os.path.join(_BACKTEST, "candidates", "cumulative_volume_delta_divergence_1638", "study_manifest.json")
DOC_SUPPORTS = {
    "perps base-tier taker and maker fee": "perps base-tier taker and maker fee",
    "perps minimum order value": "perps minimum order value",
    "lot size from meta szDecimals": "lot size from meta szDecimals",
    "open-interest source: activeAssetCtx carries PerpsAssetCtx.openInterest and no event timestamp":
        "oracle source: activeAssetCtx carries PerpsAssetCtx.oraclePx and no event timestamp, so oracle freshness is receipt age only",
    "historical asset contexts exist only in a requester-pays archive with possible gaps":
        "historical asset contexts exist only in a requester-pays archive with possible gaps and no receipt times",
}


def _hour_ceil(ms: int) -> int:
    return -(-ms // 3_600_000) * 3_600_000


def _iso(ms: int) -> str:
    return pd.Timestamp(int(ms), unit="ms", tz="UTC").strftime("%Y-%m-%dT%H:%M:%SZ")


MIN_TEST_HOURS = 2
TRAIN_SHARE = 0.6
EARLY_STOP_FLOOR_HOURS = 12


def _recording_length_deviation(r: dict) -> list:
    hours = (r["closed_ms"] - r["started_ms"]) / 3_600_000
    if hours >= EARLY_STOP_FLOOR_HOURS:
        return []
    return [f"- Deviation from the frozen specification: the recording ran {hours:.2f} h, {EARLY_STOP_FLOOR_HOURS - hours:.2f} h "
            f"below the {EARLY_STOP_FLOOR_HOURS} h early-stop floor in `candidate_spec.json` `data_plan.planned_length`; the stop "
            "came before any scoring, because the windows derive from the close time, and the frozen window rule ran unchanged "
            "on the recorded start and close times."]


def plan_windows(start_ms: int, close_ms: int) -> tuple:
    first = _hour_ceil(start_ms)
    last = close_ms // 3_600_000 * 3_600_000
    usable = (last - first) // 3_600_000 - 1
    train_hours = int(usable * TRAIN_SHARE)
    if usable - train_hours < MIN_TEST_HOURS or train_hours < 1:
        raise SystemExit(f"the recording leaves {usable} usable whole hours; the frozen rule needs at least "
                         f"{MIN_TEST_HOURS} test hours after a train window")
    train_start = first + 3_600_000
    return train_start, train_start + train_hours * 3_600_000, last


def init_manifest(start_ms: int, close_ms: int) -> dict:
    if os.path.exists(MANIFEST):
        raise SystemExit(f"{MANIFEST} exists; the frozen manifest is never rewritten by init-manifest")
    train_start, split, end = plan_windows(start_ms, close_ms)
    with open(DOC_SOURCE) as fh:
        docs = [dict(d, supports=DOC_SUPPORTS[d["supports"]]) for d in json.load(fh)["venue_reference"]["documents"]
                if d["supports"] in DOC_SUPPORTS]
    raw = {
        "schema": om.SCHEMA_V6,
        "study": "perpetual_premium_reversion_1639",
        "venue": "hyperliquid",
        "provenance": {"kind": "venue", "label": "Hyperliquid mainnet perpetual 1m candles from the public info API "
                                                 "(candleSnapshot, closed bars only); synchronized bbo and activeAssetCtx "
                                                 "samples from a forward recording of the public WebSocket streams "
                                                 "(receipt cutoff on a 60 s grid)"},
        "interval": "1m",
        "warmup_bars": 400,
        "funding_entry_input": True,
        "windows": {
            "train": {"start": _iso(train_start), "end": _iso(split),
                      "role": "selection: the parameter grid and the frozen selection rule read only this window"},
            "test": {"start": _iso(split), "end": _iso(end),
                     "role": "held-out: scored once with the frozen selection"},
        },
        "acquisition": {"source": "POST https://api.hyperliquid.xyz/info candleSnapshot + fundingHistory "
                                  "(shared_tools/funding_fetcher.py); premium samples are not acquirable here and come "
                                  "only from run_study.py prepare",
                        "start": _iso(train_start - 86_400_000), "end": _iso(end + 15 * 60_000)},
        "costs": {"taker_fee_pct": 0.00045, "maker_fee_pct": 0.00015, "slippage_bps": 1.0, "min_notional_usd": 10.0,
                  "min_notional_margin": 0.03},
        "datasets": [
            {"coin": coin, "symbol": f"{coin}/USDT", "size_decimals": dec, "half_spread_bps": 0.0,
             "candles": {"path": f"data/{coin}_1m_candles.csv.gz", "sha256": ""},
             "funding": {"path": f"data/{coin}_funding.csv.gz", "sha256": ""},
             "perp_premium": {"path": f"data/{coin}_perp_premium.json.gz", "sha256": ""}}
            for coin, dec in (("BTC", 5), ("ETH", 4))],
        "venue_reference": {"meta": {"path": "data/venue_meta.json", "sha256": ""},
                            "l2_snapshot": {"path": "data/venue_l2_snapshot.json", "sha256": ""},
                            "documents": docs},
    }
    _write_manifest(raw)
    return {"windows": raw["windows"], "acquisition": raw["acquisition"]}


def prepare(run_dir: str) -> dict:
    loaded = pr.load_premium_recording(run_dir)
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
    reloaded = pr.load_premium_recording(RECORDING_DIR)
    raw = _raw_manifest()
    out = {"run_id": loaded["report"]["run_id"], "economic_evidence": loaded["report"]["economic_evidence"],
           "refusals": loaded["report"]["refusals"], "events_compared": loaded["report"]["events_compared"], "series": {}}
    for ds in raw["datasets"]:
        coin = ds["coin"]
        series = reloaded["series"][coin]
        if series["samples_sha256"] != loaded["series"][coin]["samples_sha256"]:
            raise SystemExit(f"{coin}: the compressed copy rebuilds a different series")
        target = os.path.join(_HERE, ds["perp_premium"]["path"])
        ds["perp_premium"]["sha256"] = pr.write_premium_series(series, target)
        out["series"][coin] = {"samples": len(series["samples"]), "stats": series["stats"],
                               "quote_latency_ms": series["quote_latency_ms"], "samples_sha256": series["samples_sha256"]}
    raw["recording"] = {"path": "data/recording/run.manifest.json",
                        "sha256": _sha(os.path.join(RECORDING_DIR, "run.manifest.json")),
                        "run_id": loaded["report"]["run_id"]}
    _write_manifest(raw)
    return out


def acquire() -> dict:
    return om.acquire(MANIFEST, write_hashes=True)


def _quotes_in(quotes: list, start_ms: int, end_ms: int) -> list:
    return [q for q in quotes if start_ms <= q[0] < end_ms]


def finalize() -> dict:
    raw = _raw_manifest()
    loaded = pr.load_premium_recording(RECORDING_DIR, keep_quotes=True)
    win = raw["windows"][SELECTION_WINDOW]
    start, end = om._ts_ms(win["start"]), om._ts_ms(win["end"])
    out = {}
    for ds in raw["datasets"]:
        halves = []
        for q in _quotes_in(loaded["quotes"][ds["coin"]], start, end):
            bid, ask = float(q[2]), float(q[4])
            halves.append((ask - bid) / 2 / ((ask + bid) / 2) * 10_000)
        if not halves:
            raise SystemExit(f"{ds['coin']}: no accepted recorded quote in the train window")
        ds["half_spread_bps"] = round(float(statistics.median(halves)), 4)
        out[ds["coin"]] = {"half_spread_bps": ds["half_spread_bps"], "quotes": len(halves)}
    _write_manifest(raw)
    return out


def _pinned(manifest: dict, ref: dict, label: str) -> str:
    path = om._resolve(manifest["base_dir"], ref["path"])
    if not os.path.isfile(path) or _sha(path) != ref.get("sha256"):
        raise om.ManifestError(f"{label}: pinned file {ref['path']} is missing or its sha256 differs")
    return path


def verify_recording(manifest: dict):
    rec = manifest["raw"].get("recording") or {}
    path = _pinned(manifest, rec, "recording")
    loaded = pr.load_premium_recording(os.path.dirname(path), keep_quotes=True)
    report = loaded["report"]
    out = {k: report[k] for k in ("run_id", "recorder_version", "quote_frames", "context_frames", "malformed_frames",
                                  "dropped", "events_compared", "economic_evidence", "refusals", "connections",
                                  "max_quote_silence_ms", "max_context_silence_ms", "started_ms", "closed_ms")}
    out["sessions"] = report["sessions"]
    out["segments"] = report["segments"]
    out["series"] = {}
    for ds in manifest["datasets"]:
        committed = om.load_perp_premium(ds)
        rebuilt = loaded["series"][ds["coin"]]
        if committed["samples_sha256"] != rebuilt["samples_sha256"]:
            raise om.ManifestError(f"{ds['key']}: the committed premium series differs from the raw recording")
        reasons = {}
        for s in rebuilt["samples"]:
            if s["status"] != "sampled":
                reasons[s.get("reason") or ""] = reasons.get(s.get("reason") or "", 0) + 1
        out["series"][ds["coin"]] = {"samples": len(rebuilt["samples"]), "stats": rebuilt["stats"],
                                     "quote_latency_ms": rebuilt["quote_latency_ms"],
                                     "samples_sha256": rebuilt["samples_sha256"],
                                     "non_sampled_reasons": dict(sorted(reasons.items())),
                                     "revoked": sum(1 for s in rebuilt["samples"] if s.get("invalid_ms") is not None),
                                     "gaps": rebuilt["gaps"]}
    return out, loaded["quotes"]


def _contract(spec: dict) -> dict:
    ex = spec["exit_and_risk"]
    return {"direction": ex["direction"],
            "close_strategies": [dict(ex["close_strategy"], params=dict(ex["close_strategy"]["params"]))],
            "stop_loss_atr_mult": 1.0, "comparison_mode": COMPARISON_MODE}


def arm(spec: dict, name: str, params: dict) -> dict:
    return {"name": name, "params": dict(params), **_contract(spec)}


def _grid(defaults: dict) -> list:
    specs = [(k, list(v)) for k, v in DEFAULT_PARAM_RANGES[CANDIDATE].items()]
    return expand_sweep(dict(defaults), specs)


def candidate_frame(reg, manifest, dataset, window_name, params):
    frame, win, _ = om.window_frame(manifest, dataset, window_name)
    obs, _ = om.attach_perp_premium(manifest, dataset, win)
    out = reg.apply_strategy(CANDIDATE, frame, {**params, "premium_observations": obs})
    return frame, win, out


def valid_share(reg, manifest, window_name, params) -> dict:
    out = {}
    for ds in manifest["datasets"]:
        _, win, cand = candidate_frame(reg, manifest, ds, window_name, params)
        scored = om.slice_window(cand, win)
        valid = scored["ppr_reason"].isin(DATA_VALID_REASONS)
        out[ds["key"]] = {"bars": int(len(scored)), "valid_bars": int(valid.sum()),
                          "share": round(float(valid.mean()), 4) if len(scored) else 0.0,
                          "reasons": {str(k): int(v) for k, v in scored["ppr_reason"].value_counts().items()}}
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
        for ts, r in scored[scored["signal"] != 0][list(EVIDENCE_COLUMNS)].iterrows():
            entry = {"dataset": ds["key"], "bar_open": str(ts)}
            for c in EVIDENCE_COLUMNS:
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
    return {
        "datasets": len(per),
        "mean_return_pct": round(statistics.mean(v["return_pct"] for v in per.values()), 4),
        "mean_sharpe": round(statistics.mean(v["sharpe"] for v in per.values()), 4),
        "mean_ddadj": round(statistics.mean(v["ddadj"] for v in per.values()), 4),
        "worst_max_dd_pct": round(min(v["max_dd_pct"] for v in per.values()), 4),
        "net_pnl_usd": round(sum(v["net_pnl_usd"] for v in per.values()), 4),
        "fees_usd": round(sum(v["fees_usd"] for v in per.values()), 4),
        "funding_pnl_usd": round(sum(v["funding_pnl_usd"] for v in per.values()), 6),
        "mean_turnover": round(statistics.mean(v["turnover"] for v in per.values()), 4),
        "positions": len(positions),
        "long_positions": sum(1 for p in positions if p["side"] == "long"),
        "short_positions": sum(1 for p in positions if p["side"] == "short"),
        "top_dataset_abs_pnl_share": round(max(abs_ds) / sum(abs_ds), 4) if sum(abs_ds) > 0 else None,
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


def _register_masked(reg, base: str, coin_masks: dict) -> str:
    name = f"{base}__premium_eligible"
    entry = reg.STRATEGY_REGISTRY[base]
    base_fn = entry["fn"]

    def masked(df, **params):
        prep = coin_masks.get("prep")
        out = base_fn(prep(df) if prep else df, **params)
        allowed = coin_masks["current"]
        keep = np.array([bool(allowed.get(int(ts.value // 1_000_000), False)) for ts in out.index])
        out["signal"] = np.where(keep, out["signal"], 0)
        return out

    reg.STRATEGY_REGISTRY[name] = {**entry, "fn": masked, "param_validator": None, "description": f"{base} masked"}
    return name


def masked_baseline(reg, spec, manifest, datasets, name, params, window_name, cost, memo, selected):
    holder = {"current": {}, "prep": None}
    masked_name = _register_masked(reg, name, holder)
    legs = {}
    for ds, pair in zip(manifest["datasets"], datasets):
        _, win, cand = candidate_frame(reg, manifest, ds, window_name, selected)
        if name in om.FUNDING_ENTRY_INPUT_STRATEGIES:
            holder["prep"] = lambda df, ds=ds, win=win: om.attach_funding_entry_input(df, ds, win)[0]
        holder["current"] = {int(ts.value // 1_000_000): reason in DATA_VALID_REASONS
                             for ts, reason in cand["ppr_reason"].items()}
        score = evaluate_window(reg, arm(spec, masked_name, params), [pair], window_name,
                                spec["exit_and_risk"]["capital_usd"], memo, manifest=manifest,
                                cost_multiplier=cost, incumbent_contract=_contract(spec))
        legs.update(_legs(score))
    reg.STRATEGY_REGISTRY.pop(masked_name, None)
    return summarize(legs)


def event_result(per_coin: list, case: str, capital: float) -> dict:
    trades, refusals, open_positions = [], [], []
    for coin, res in per_coin:
        trades += res["trades"]
        refusals += [dict(x, coin=coin) for x in res["refusals"]]
        open_positions += res["open_positions"]
    lat = er.COST_CASES[case]["latency_ms"]
    violations = [t for t in trades if t["entry_ms"] < t["decision_ms"] + lat or t["exit_ms"] < t["exit_decision_ms"] + lat
                  or t["exit_ms"] < t["entry_ms"]
                  or any(f["recv_ms"] < t["exit_decision_ms"] + lat or f["recv_ms"] < t["entry_ms"] for f in t["exit_fills"])]
    violations += [p for p in open_positions if p["entry_ms"] < p["decision_ms"] + lat
                   or any(f["recv_ms"] < p["exit_decision_ms"] + lat or f["recv_ms"] < p["entry_ms"] for f in p["exit_fills"])]
    return {"summary": er.summarize(trades, capital, open_positions), "trades": trades, "refusals": refusals,
            "open_positions": open_positions, "cost": er.COST_CASES[case],
            "fill_timing_violations": len(violations)}


def event_payload(ev: dict) -> dict:
    return {"summary": ev["summary"], "refusals": ev["refusals"], "cost": ev["cost"],
            "fill_timing_violations": ev["fill_timing_violations"], "trades": ev["trades"],
            "open_positions": ev["open_positions"]}


def event_study(reg, spec, manifest, quotes, window_name, params, case) -> dict:
    ex = spec["exit_and_risk"]
    per_coin = []
    for ds in manifest["datasets"]:
        _, win, cand = candidate_frame(reg, manifest, ds, window_name, params)
        cand = ensure_atr_indicator(cand)
        scored = om.slice_window(cand, win)
        decisions = []
        for _, r in scored.iterrows():
            if not np.isfinite(r["ppr_deadline_ms"]):
                continue
            decisions.append({"decision_ms": int(r["ppr_deadline_ms"]), "signal": int(r["signal"]),
                              "atr": float(r["atr"]) if np.isfinite(r["atr"]) else None,
                              "premium": _clean(r["ppr_premium"]), "z_extreme": _clean(r["ppr_z_extreme"])})
        funding = om.load_funding(ds)
        fund_rows = [] if funding is None else [(int(t), float(x)) for t, x in zip(funding["timestamp"], funding["rate"])]
        res = er.replay_coin(ds["coin"], decisions, er.QuoteTape(quotes[ds["coin"]]), fund_rows, ds["size_decimals"],
                             ex["capital_usd"], ex["close_strategy"]["params"]["max_bars"], manifest["interval_ms"],
                             1.0, er.COST_CASES[case])
        per_coin.append((ds["coin"], res))
    return event_result(per_coin, case, ex["capital_usd"])


def verdict(event_x1, event_x2, cand_base, baselines, masked, shares, recording, candle_cov) -> dict:
    checks = {
        "recording_is_economic_evidence": bool(recording["economic_evidence"]),
        "sample_sufficient": event_x1["summary"]["positions"] >= MIN_TEST_POSITIONS,
        "premium_coverage": all(v["share"] >= MIN_VALID_SHARE for v in shares.values()),
        "candle_coverage_complete": candle_cov,
        "event_net_positive_x1": event_x1["summary"]["net_pnl_usd"] > 0,
        "event_net_positive_x2": event_x2["summary"]["net_pnl_usd"] > 0,
        "candle_net_positive_x1": cand_base.get("net_pnl_usd", 0) > 0,
        "beats_baselines_ddadj": all(cand_base.get("mean_ddadj", 0) > b.get("mean_ddadj", 0)
                                     for b in list(baselines.values()) + list(masked.values())),
        "not_concentrated": (event_x1["summary"].get("top_coin_abs_pnl_share") or 1.0) <= MAX_COIN_PNL_SHARE,
        "event_fills_after_decision_and_latency": event_x1["fill_timing_violations"] == 0 and event_x2["fill_timing_violations"] == 0,
        "event_positions_resolved": event_x1["summary"]["complete"] and event_x2["summary"]["complete"],
    }
    gates = ("recording_is_economic_evidence", "sample_sufficient", "premium_coverage", "candle_coverage_complete",
             "event_fills_after_decision_and_latency", "event_positions_resolved")
    missing = []
    if not checks["recording_is_economic_evidence"]:
        missing.append("the recording is incomplete or damaged: " + "; ".join(recording["refusals"]))
    if not checks["sample_sufficient"]:
        missing.append(f"{event_x1['summary']['positions']} held-out event-replay positions < {MIN_TEST_POSITIONS}")
    if not checks["premium_coverage"]:
        low = sorted(f"{k} ({v['share']})" for k, v in shares.items() if v["share"] < MIN_VALID_SHARE)
        missing.append(f"valid-premium bar share below {MIN_VALID_SHARE} on {', '.join(low)}")
    if not checks["candle_coverage_complete"]:
        missing.append("held-out candle coverage is incomplete")
    if not checks["event_fills_after_decision_and_latency"]:
        missing.append("an event-replay fill preceded its decision plus latency")
    if not checks["event_positions_resolved"]:
        missing.append(f"held-out event replay is incomplete: {event_x1['summary']['open_positions']} (x1) and "
                       f"{event_x2['summary']['open_positions']} (x2) positions still open at the end of the recording, "
                       "so the closed-position results cannot decide a pass or fail")
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
    recording, quotes = verify_recording(manifest)
    reg = load_registry("futures")
    datasets = manifest_datasets(manifest)
    contract = _contract(spec)
    capital = spec["exit_and_risk"]["capital_usd"]
    memo: dict = {}

    def score(name, params, window, cost):
        return evaluate_window(reg, arm(spec, name, params), datasets, window, capital, memo, manifest=manifest,
                               cost_multiplier=cost, incumbent_contract=contract)

    defaults = dict(reg.STRATEGY_REGISTRY[CANDIDATE]["default_params"])
    train_rows = []
    for label, params in _grid(defaults):
        summary = summarize(_legs(score(CANDIDATE, params, SELECTION_WINDOW, BASE_COST)))
        summary.pop("per_dataset", None)
        train_rows.append({"label": label, "params": params, "summary": summary})
    choice = select(train_rows, len(datasets))
    selected = choice["params"] if choice["selected"] else defaults

    cand_score = score(CANDIDATE, selected, HELD_OUT_WINDOW, BASE_COST)
    held = {CANDIDATE: {"params": selected, "base": summarize(_legs(cand_score)),
                        "stress": summarize(_legs(score(CANDIDATE, selected, HELD_OUT_WINDOW, STRESS_COST))),
                        "m1_verdict": cand_score.get("verdict"), "m1_bar_mean_ddadj": cand_score.get("mean_bar_ddadj")}}
    masked = {}
    train_held = {CANDIDATE: summarize(_legs(score(CANDIDATE, selected, SELECTION_WINDOW, BASE_COST)))}
    for name, params in BASELINES.items():
        bparams = {**reg.STRATEGY_REGISTRY[name]["default_params"], **params}
        held[name] = {"params": bparams, "base": summarize(_legs(score(name, bparams, HELD_OUT_WINDOW, BASE_COST))),
                      "stress": summarize(_legs(score(name, bparams, HELD_OUT_WINDOW, STRESS_COST)))}
        masked[name] = masked_baseline(reg, spec, manifest, datasets, name, bparams, HELD_OUT_WINDOW, BASE_COST, memo,
                                       selected)
        train_held[name] = summarize(_legs(score(name, bparams, SELECTION_WINDOW, BASE_COST)))

    shares = {w: valid_share(reg, manifest, w, selected) for w in (SELECTION_WINDOW, HELD_OUT_WINDOW)}
    events = {w: {c: event_study(reg, spec, manifest, quotes, w, selected, c) for c in ("x1", "x2")}
              for w in (SELECTION_WINDOW, HELD_OUT_WINDOW)}
    candle_cov = True
    window_coverage = {}
    for ds in manifest["datasets"]:
        window_coverage[ds["key"]] = {}
        for w in (SELECTION_WINDOW, HELD_OUT_WINDOW):
            _, win, cov = om.window_frame(manifest, ds, w)
            entry_cov = om.funding_coverage(om.load_funding(ds), win["start_ms"], win["end_ms"])
            window_coverage[ds["key"]][w] = {"candles": cov, "funding": entry_cov,
                                             "perp_premium": pr.coverage_report(om.load_perp_premium(ds), win["start_ms"], win["end_ms"])}
            if w == HELD_OUT_WINDOW and not cov.get("complete", True):
                candle_cov = False

    variants = []
    positive_train = 0
    for row in train_rows:
        test_summary = summarize(_legs(score(CANDIDATE, row["params"], HELD_OUT_WINDOW, BASE_COST)))
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
        "issue": 1639,
        "spec_sha256": _sha(SPEC_PATH),
        "manifest": {
            "path": os.path.relpath(manifest["path"], _REPO),
            "sha256": _sha(manifest["path"]),
            "schema": manifest["schema"],
            "provenance": manifest["provenance"],
            "interval": manifest["interval"],
            "warmup_bars": manifest["warmup_bars"],
            "funding_entry_input": manifest["funding_entry_input"],
            "windows": {k: {"start": v["start"], "end": v["end"], "role": v["role"]} for k, v in manifest["windows"].items()},
            "costs": manifest["costs"],
            "datasets": [{"key": d["key"], "size_decimals": d["size_decimals"], "half_spread_bps": d["half_spread_bps"],
                          "candles_sha256": d["candles"]["sha256"],
                          "funding_sha256": d["funding"]["sha256"] if d["funding"] else None,
                          "perp_premium_sha256": d["perp_premium"]["sha256"]} for d in manifest["datasets"]],
            "venue_reference": {"meta_sha256": manifest["venue_reference"]["meta"]["sha256"],
                                "documents": manifest["venue_reference"]["documents"]},
            "recording": manifest["raw"].get("recording"),
        },
        "recording": recording,
        "window_coverage": window_coverage,
        "source_sha256": {p: _sha(os.path.join(_REPO, p)) for p in SOURCE_FILES},
        "protocol": {
            "contract": contract,
            "capital_usd": capital,
            "sizing": spec["exit_and_risk"]["sizing"],
            "selection_rule": spec["selection_rule"],
            "min_train_positions": MIN_TRAIN_POSITIONS,
            "verdict_criteria": spec["verdict_criteria"],
            "cost_multipliers": {"base": BASE_COST, "stress": STRESS_COST},
            "event_replay": spec["execution_models"]["event_replay"],
            "event_cost_cases": er.COST_CASES,
            "baselines": spec["baselines"],
        },
        "selection": {"grid": {k: list(v) for k, v in DEFAULT_PARAM_RANGES[CANDIDATE].items()},
                      "train_sweep": train_rows, "choice": choice, "selected_params": selected,
                      "stability": {"grid_points": len(train_rows), "positive_train_net": positive_train,
                                    "positive_share": round(positive_train / len(train_rows), 4) if train_rows else None}},
        "valid_share": shares,
        "held_out": held,
        "held_out_masked_baselines": masked,
        "train_selected": train_held,
        "event_replay": {w: {c: event_payload(v) for c, v in per.items()} for w, per in events.items()},
        "held_out_entries": entry_diagnostics(reg, manifest, HELD_OUT_WINDOW, selected),
        "train_entries": entry_diagnostics(reg, manifest, SELECTION_WINDOW, selected),
        "variants": variants,
    }
    result["verdict"] = verdict(events[HELD_OUT_WINDOW]["x1"], events[HELD_OUT_WINDOW]["x2"], held[CANDIDATE]["base"],
                                {k: held[k]["base"] for k in BASELINES}, masked, shares[HELD_OUT_WINDOW], recording,
                                candle_cov)
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


def event_completeness_lines(event_replay: dict) -> list:
    lines = ["", "## Event replay completeness (positions open at the end of the recording)", "",
             "The table above counts closed positions only. An open position keeps its entry fee, completed exit "
             "fills, realized PnL on the closed quantity and accrued funding here; its remaining quantity is unpriced.",
             "", "| Window | Cost | Complete | Open positions | Remaining qty | Fees $ | Realized gross $ | Funding $ | Realized net $ |",
             "|---|---|---|---|---|---|---|---|---|"]
    rows = []
    for w in (SELECTION_WINDOW, HELD_OUT_WINDOW):
        for c in ("x1", "x2"):
            ev = event_replay[w][c]
            s = ev["summary"]
            qty = ", ".join(f"{k} {v}" for k, v in s["open_remaining_qty"].items()) or "-"
            lines.append(f"| {w} | {c} | {_fmt(s['complete'])} | {s['open_positions']} | {qty} | "
                         f"{_fmt(s['open_fees_usd'])} | {_fmt(s['open_realized_gross_usd'])} | "
                         f"{_fmt(s['open_funding_usd'], 4)} | {_fmt(s['open_realized_net_usd'])} |")
            rows += [(w, c, p) for p in ev["open_positions"]]
    if not rows:
        return lines + ["", "No position was open at the end of the recording."]
    lines += ["", "| Window | Cost | Coin | Side | Decision | Entry | Entry px | Entered qty | Exit fills | Closed qty | "
                  "Remaining qty | Entry fee $ | Exit fees $ | Realized gross $ | Funding $ | Funding through |",
              "|---|---|---|---|---|---|---|---|---|---|---|---|---|---|---|---|"]
    for w, c, p in rows:
        lines.append(f"| {w} | {c} | {p['coin']} | {p['side']} | {_ts(p['decision_ms'])} | {_ts(p['entry_ms'])} | "
                     f"{_fmt(p['entry_px'])} | {p['qty']} | {len(p['exit_fills'])} | {p['closed_qty']} | "
                     f"{p['remaining_qty']} | {_fmt(p['entry_fees_usd'], 4)} | {_fmt(p['exit_fees_usd'], 4)} | "
                     f"{_fmt(p['realized_gross_usd'], 4)} | {_fmt(p['funding_usd'], 4)} | {_ts(p['funding_through_ms'])} |")
    return lines


def render(result: dict) -> str:
    v = result["verdict"]
    m = result["manifest"]
    r = result["recording"]
    lines = [
        "# Perpetual premium mean reversion: recorded held-out result (#1639)",
        "",
        f"Verdict: **{v['outcome'].upper()}**. {v['reason']}. {v['live_status']}.",
        "",
        "Generated by `run_study.py` from `results.json`; do not edit by hand.",
        "",
        "## Inputs",
        "",
        f"- Manifest `{m['path']}` ({m['schema']}) sha256 `{m['sha256']}`; funding entry input for `funding_skew`: "
        f"{_fmt(m['funding_entry_input'])}",
        f"- Specification `candidate_spec.json` sha256 `{result['spec_sha256']}`",
        f"- Provenance: {m['provenance']['label']}",
        f"- Interval {m['interval']}, warm-up {m['warmup_bars']} bars (not scored)",
    ]
    for name, w in sorted(m["windows"].items(), key=lambda kv: kv[1]["start"]):
        lines.append(f"- Window `{name}` [{w['start']}, {w['end']}): {w['role']}")
    c = m["costs"]
    lines.append(f"- Candle-model costs: taker {c['taker_fee_pct']*100:.3f}%, slippage {c['slippage_bps']} bps per side "
                 f"plus the measured per-coin half spread, minimum order ${c['min_notional_usd']:.0f}, funding booked "
                 "hourly for every arm")
    for d in m["datasets"]:
        lines.append(f"- `{d['key']}`: szDecimals {d['size_decimals']}, measured half spread {d['half_spread_bps']} bps, "
                     f"candles `{d['candles_sha256'][:16]}`, funding `{(d['funding_sha256'] or '')[:16]}`, premium series "
                     f"`{d['perp_premium_sha256'][:16]}`")
    lines += [
        "",
        "## Recording and replay",
        "",
        f"- Run `{r['run_id']}` (recorder `{r['recorder_version']}`), {_ts(r['started_ms'])} to {_ts(r['closed_ms'])}: "
        f"{len(r['segments'])} hashed segments, {r['quote_frames']} bbo frames, {r['context_frames']} activeAssetCtx frames, "
        f"{r['malformed_frames']} malformed frames, {r['dropped']} dropped records, sessions {r['sessions']}.",
        *_recording_length_deviation(r),
        f"- Independent Python replay reproduced every frame outcome and all {r['events_compared']} recorder sample and "
        f"revocation events. Economic evidence: {_fmt(r['economic_evidence'])}"
        + (f" ({'; '.join(r['refusals'])})" if r["refusals"] else "") + ".",
        "- Connection changes: " + (", ".join(f"{x['state']} {_ts(x['recv_ms'])} (session {x['session']})"
                                              for x in r["connections"]) or "none") + ".",
        "",
        "| Coin | Samples | Sampled | Missing | Invalid | Revoked | Quote frames | One-sided | Duplicates | Conflicts | Out of order | Rejected | Quote latency p50/p99/max ms | Max quote / context silence ms | Non-sampled reasons |",
        "|---|---|---|---|---|---|---|---|---|---|---|---|---|---|---|",
    ]
    for coin, sr in sorted(r["series"].items()):
        st, lat = sr["stats"], sr["quote_latency_ms"]
        reasons = ", ".join(f"{k} {n}" for k, n in sr["non_sampled_reasons"].items()) or "-"
        lines.append(f"| {coin} | {sr['samples']} | {st['sampled']} | {st['missing']} | {st['invalid']} | {sr['revoked']} | "
                     f"{st['quote_frames']} | {st['quote_one_sided']} | {st['quote_duplicates']} | {st['quote_conflicts']} | "
                     f"{st['quote_out_of_order']} | {st['rejected']} | {lat['p50']}/{lat['p99']}/{lat['max']} | "
                     f"{r['max_quote_silence_ms'].get(coin)} / {r['max_context_silence_ms'].get(coin)} | {reasons} |")
    p = result["protocol"]
    ct = p["contract"]
    lines += [
        "",
        "## Protocol",
        "",
        f"- Every arm: direction `{ct['direction']}`, close `time_stop` (max_bars {ct['close_strategies'][0]['params']['max_bars']}), "
        f"entry-ATR stop x{ct['stop_loss_atr_mult']}, capital ${p['capital_usd']:.0f}; {p['sizing']}. M1 incumbents use the same contract.",
        f"- Close comparison mode `{ct['comparison_mode']}`: time_stop reads the simulator's held-bar count, which no live "
        "close context supplies, so this is research evidence with incomplete live close parity.",
        f"- Baselines: {json.dumps(p['baselines'], sort_keys=True)}",
        f"- Event replay: {p['event_replay']}",
        f"- Selection: {p['selection_rule']}",
        f"- Selection outcome: {result['selection']['choice']['rule_outcome']}; scored params "
        f"`{json.dumps(result['selection']['selected_params'], sort_keys=True)}`",
        f"- Parameter stability: {result['selection']['stability']['positive_train_net']} of "
        f"{result['selection']['stability']['grid_points']} grid points had positive train net PnL.",
        "",
        "## Valid-premium share (selected params)",
        "",
        "| Window | Dataset | Scored bars | Valid bars | Share | Reasons |",
        "|---|---|---|---|---|---|",
    ]
    for w in (SELECTION_WINDOW, HELD_OUT_WINDOW):
        for ds, sh in sorted(result["valid_share"][w].items()):
            reasons = ", ".join(f"{k} {n}" for k, n in sorted(sh["reasons"].items()))
            lines.append(f"| {w} | {ds} | {sh['bars']} | {sh['valid_bars']} | {_fmt(sh['share'], 3)} | {reasons} |")
    lines += ["", "## Held-out comparison (candle model, M1 runner)", "",
              "| Arm | Cost | Positions (L/S) | Net $ | Fees $ | Funding $ | Mean ret % | Mean DDadj | Worst DD % | Turnover | Top coin share |",
              "|---|---|---|---|---|---|---|---|---|---|---|"]

    def row(label, cost, sm):
        if not sm.get("datasets"):
            return f"| {label} | {cost} | 0 | - | - | - | - | - | - | - | - |"
        return (f"| {label} | {cost} | {sm['positions']} ({sm['long_positions']}/{sm['short_positions']}) | "
                f"{_fmt(sm['net_pnl_usd'])} | {_fmt(sm['fees_usd'])} | {_fmt(sm['funding_pnl_usd'], 4)} | "
                f"{_fmt(sm['mean_return_pct'], 3)} | {_fmt(sm['mean_ddadj'], 3)} | {_fmt(sm['worst_max_dd_pct'], 3)} | "
                f"{_fmt(sm['mean_turnover'])} | {_fmt(sm['top_dataset_abs_pnl_share'])} |")
    arms = [CANDIDATE] + list(BASELINES)
    for name in arms:
        for label in ("base", "stress"):
            lines.append(row(f"`{name}`", label, result["held_out"][name][label]))
    for name in BASELINES:
        lines.append(row(f"`{name}` masked to valid-premium bars", "base", result["held_out_masked_baselines"][name]))
    lines += ["", "## Event replay on recorded quotes (candidate, selected params)", "",
              "| Window | Cost | Positions (L/S) | Net $ | Gross $ | Fees $ | Funding $ | Max DD $ | Win rate | Delayed exits | Refused entries | Top coin share |",
              "|---|---|---|---|---|---|---|---|---|---|---|---|"]
    for w in (SELECTION_WINDOW, HELD_OUT_WINDOW):
        for c in ("x1", "x2"):
            ev = result["event_replay"][w][c]
            s = ev["summary"]
            lines.append(f"| {w} | {c} | {s['positions']} ({s['long_positions']}/{s['short_positions']}) | "
                         f"{_fmt(s['net_pnl_usd'])} | {_fmt(s['gross_usd'])} | {_fmt(s['fees_usd'])} | "
                         f"{_fmt(s['funding_usd'], 4)} | {_fmt(s['max_dd_usd'])} | {_fmt(s['win_rate'], 3)} | "
                         f"{s.get('delayed_exits', 0)} | {len(ev['refusals'])} | {_fmt(s['top_coin_abs_pnl_share'])} |")
    lines += event_completeness_lines(result["event_replay"])
    lines += ["", "## Train window (selected params, candle model)", "",
              "| Arm | Positions (L/S) | Net $ | Mean DDadj |", "|---|---|---|---|"]
    for name in arms:
        sm = result["train_selected"][name]
        if not sm.get("datasets"):
            lines.append(f"| `{name}` | 0 | - | - |")
            continue
        lines.append(f"| `{name}` | {sm['positions']} ({sm['long_positions']}/{sm['short_positions']}) | "
                     f"{_fmt(sm['net_pnl_usd'])} | {_fmt(sm['mean_ddadj'], 3)} |")
    for title, key in (("Held-out entries", "held_out_entries"), ("Train entries", "train_entries")):
        lines += ["", f"## {title} (candidate, selected params)", ""]
        events = result[key]
        if not events:
            lines.append("None.")
            continue
        lines += ["| Dataset | Bar open | Side | Premium bps | Extreme bps | Mean bps | Std bps | z extreme | Spread bps | Bid / Ask | Oracle | Window sha |",
                  "|---|---|---|---|---|---|---|---|---|---|---|---|"]
        for e in events:
            bp = lambda x: None if x is None else x * 10_000
            lines.append(f"| {e['dataset']} | {e['bar_open']} | {e['ppr_side']} | {_fmt(bp(e['ppr_premium']), 3)} | "
                         f"{_fmt(bp(e['ppr_extreme_premium']), 3)} | {_fmt(bp(e['ppr_mean']), 3)} | {_fmt(bp(e['ppr_std']), 3)} | "
                         f"{_fmt(e['ppr_z_extreme'], 3)} | {_fmt(e['ppr_spread_bps'], 3)} | {_fmt(e['ppr_bid'], 2)} / "
                         f"{_fmt(e['ppr_ask'], 2)} | {_fmt(e['ppr_oracle'], 2)} | `{(e['ppr_window_sha256'] or '')[:12]}` |")
    lines += ["", "## Verdict checks", ""]
    for k, ok in sorted(v["checks"].items()):
        lines.append(f"- {k}: {_fmt(ok)}")
    lines += ["", "## All grid variants (train selection, held-out outcome, candle model)", "",
              "| Variant | Train positions | Train net $ | Train DDadj | Held-out positions | Held-out DDadj | Held-out net $ |",
              "|---|---|---|---|---|---|---|"]
    for rw in result["variants"]:
        lines.append(f"| {rw['label']} | {rw['train_positions']} | {_fmt(rw['train_net_pnl_usd'])} | "
                     f"{_fmt(rw['train_mean_ddadj'], 3)} | {rw['test_positions']} | {_fmt(rw['test_mean_ddadj'], 3)} | "
                     f"{_fmt(rw['test_net_pnl_usd'])} |")
    lines += ["", "## Source hashes", ""]
    for path, digest in sorted(result["source_sha256"].items()):
        lines.append(f"- `{path}` `{digest}`")
    lines.append("")
    return "\n".join(lines)


def _refuse_foreign_results(json_out: str) -> None:
    if not os.path.exists(json_out):
        return
    with open(json_out) as fh:
        pinned = json.load(fh).get("source_sha256") or {}
    current = {p: _sha(os.path.join(_REPO, p)) for p in SOURCE_FILES}
    changed = sorted(p for p in set(pinned) | set(current) if pinned.get(p) != current.get(p))
    if changed:
        raise SystemExit(f"{json_out} was produced by other sources ({', '.join(changed)}); run and render never "
                         "overwrite it. Reproduce it at its source revision, or run a corrected study in a scratch "
                         "copy after removing that copy's results.json and REPORT.md (README.md, Reproduce)")


def main(argv=None) -> int:
    p = argparse.ArgumentParser(description="#1639 recorded perpetual premium study driver (research-only)")
    sub = p.add_subparsers(dest="cmd")
    prep = sub.add_parser("prepare", help="Freeze a closed premium recording into data/ and pin its series hashes")
    prep.add_argument("--run-dir", required=True)
    init = sub.add_parser("init-manifest", help="Write the frozen study manifest from the closed recording's start and close times")
    init.add_argument("--start-ms", type=int, required=True)
    init.add_argument("--close-ms", type=int, required=True)
    sub.add_parser("acquire", help="Fetch the frozen 1m candles, funding and venue meta and pin their hashes")
    sub.add_parser("finalize", help="Pin per-coin half spreads measured from recorded train-window quotes")
    runp = sub.add_parser("run", help="Score the frozen study (default)")
    runp.add_argument("--manifest", default=MANIFEST)
    runp.add_argument("--render-only", action="store_true")
    args = p.parse_args(argv)
    json_out = os.path.join(_HERE, "results.json")
    report = os.path.join(_HERE, "REPORT.md")
    if args.cmd in (None, "run"):
        _refuse_foreign_results(json_out)
    try:
        if args.cmd == "prepare":
            print(json.dumps(prepare(args.run_dir), indent=2, sort_keys=True))
            return 0
        if args.cmd == "init-manifest":
            print(json.dumps(init_manifest(args.start_ms, args.close_ms), indent=2, sort_keys=True))
            return 0
        if args.cmd == "acquire":
            print(json.dumps(acquire(), indent=2, sort_keys=True, default=str))
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
    except (om.ManifestError, pr.PremiumRecordingError) as exc:
        print(f"study error: {exc}", file=sys.stderr)
        return 1


if __name__ == "__main__":
    sys.exit(main())

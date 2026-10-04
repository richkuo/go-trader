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

import numpy as np
import pandas as pd

_HERE = os.path.dirname(os.path.abspath(__file__))
_BACKTEST = os.path.abspath(os.path.join(_HERE, "..", ".."))
_REPO = os.path.abspath(os.path.join(_BACKTEST, ".."))
sys.path.insert(0, _BACKTEST)
sys.path.insert(0, os.path.join(_REPO, "shared_tools"))

import observation_replay as orp
import offline_manifest as om
from backtester import aggregate_close_validations
from eval_windows import (INCUMBENTS, WINDOWS, evaluate_window, execution_metrics, expand_sweep,
                          leg_from_results, manifest_datasets)
from optimizer import DEFAULT_PARAM_RANGES

MANIFEST = os.path.join(_HERE, "study_manifest.json")
SPEC_PATH = os.path.join(_HERE, "candidate_spec.json")
RECORDING_DIR = os.path.join(_HERE, "data", "recording")
CANDIDATE = "open_interest_breakout"
BASELINE = "donchian_breakout"
MATCHED = "donchian_breakout_coverage_matched"
SELECTION_WINDOW = "train"
HELD_OUT_WINDOW = "test"
BASE_COST = 1.0
STRESS_COST = 2.0
MIN_TRAIN_POSITIONS = 6
MIN_TEST_POSITIONS = 30
MIN_OI_VALID_SHARE = 0.9
MAX_COIN_PNL_SHARE = 0.6
SOURCE_FILES = (
    "shared_strategies/open/open_interest_breakout.py",
    "shared_strategies/open/donchian_breakout.py",
    "shared_strategies/open/registry.py",
    "shared_tools/strategy_composition.py",
    "shared_tools/market_payload.py",
    "shared_strategies/close/time_stop.py",
    "backtest/backtester.py",
    "backtest/eval_windows.py",
    "backtest/offline_manifest.py",
    "backtest/observation_replay.py",
    "backtest/optimizer.py",
    "backtest/candidates/open_interest_breakout_1637/run_study.py",
    "backtest/candidates/open_interest_breakout_1637/candidate_spec.json",
    "scheduler/market_feed_observations.go",
    "scheduler/market_feed_recorder.go",
    "scheduler/market_feed_ws.go",
)


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


def prepare(run_dir: str) -> dict:
    loaded = orp.load_recording(run_dir)
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
    reloaded = orp.load_recording(RECORDING_DIR)
    with open(MANIFEST) as fh:
        raw = json.load(fh)
    out = {"run_id": loaded["report"]["run_id"], "series": {}}
    for ds in raw["datasets"]:
        coin = ds["coin"]
        series = reloaded["series"][coin]
        if series["samples_sha256"] != loaded["series"][coin]["samples_sha256"]:
            raise SystemExit(f"{coin}: the compressed copy rebuilds a different series")
        target = os.path.join(_HERE, ds["open_interest"]["path"])
        ds["open_interest"]["sha256"] = orp.write_series(series, target)
        out["series"][coin] = {"samples": len(series["samples"]), "gaps": series["gaps"],
                               "samples_sha256": series["samples_sha256"]}
    for doc in raw["venue_reference"]["documents"]:
        doc["quote_sha256"] = hashlib.sha256(doc["quote"].encode("utf-8")).hexdigest()
    raw["recording"] = {"path": "data/recording/run.manifest.json",
                        "sha256": _sha(os.path.join(RECORDING_DIR, "run.manifest.json")),
                        "run_id": loaded["report"]["run_id"]}
    with open(MANIFEST, "w") as fh:
        json.dump(raw, fh, indent=2)
        fh.write("\n")
    return out


def finalize() -> dict:
    with open(MANIFEST) as fh:
        raw = json.load(fh)
    with open(os.path.join(_HERE, raw["venue_reference"]["l2_snapshot"]["path"])) as fh:
        books = json.load(fh)
    for ds in raw["datasets"]:
        ds["half_spread_bps"] = round(float(books[ds["coin"]]["half_spread_bps"]), 4)
    with open(MANIFEST, "w") as fh:
        json.dump(raw, fh, indent=2)
        fh.write("\n")
    return {ds["coin"]: ds["half_spread_bps"] for ds in raw["datasets"]}


def verify_recording(manifest: dict) -> dict:
    rec = manifest["raw"].get("recording") or {}
    path = os.path.join(manifest["base_dir"], rec.get("path", ""))
    if not os.path.isfile(path) or _sha(path) != rec.get("sha256"):
        raise om.ManifestError("the pinned recording run manifest is missing or its sha256 differs")
    loaded = orp.load_recording(os.path.dirname(path))
    out = {"run_id": loaded["report"]["run_id"], "accepted": loaded["report"]["accepted"],
           "refreshed": loaded["report"]["refreshed"], "rejected": loaded["report"]["rejected"],
           "dropped": loaded["report"]["dropped"], "sessions": loaded["report"]["sessions"],
           "segments": loaded["report"]["segments"], "series": {}}
    for ds in manifest["datasets"]:
        committed = om.load_open_interest(ds)
        rebuilt = loaded["series"][ds["coin"]]
        if committed["samples_sha256"] != rebuilt["samples_sha256"]:
            raise om.ManifestError(f"{ds['key']}: the committed series differs from the raw recording")
        out["series"][ds["coin"]] = {"samples": len(rebuilt["samples"]), "gaps": rebuilt["gaps"],
                                     "samples_sha256": rebuilt["samples_sha256"]}
    return out


COMPARISON_MODE = "approximate"


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
    obs, _ = om.attach_open_interest(manifest, dataset, win)
    out = reg.apply_strategy(CANDIDATE, frame, {**params, "open_interest_observations": obs})
    return frame, win, out


def oi_valid_share(reg, manifest, window_name, params) -> dict:
    out = {}
    for ds in manifest["datasets"]:
        _, win, cand = candidate_frame(reg, manifest, ds, window_name, params)
        scored = om.slice_window(cand, win)
        out[ds["key"]] = {"bars": int(len(scored)), "oi_valid_bars": int(scored["oib_oi_valid"].sum()),
                          "share": round(float(scored["oib_oi_valid"].mean()), 4) if len(scored) else 0.0,
                          "reasons": {str(k): int(v) for k, v in scored["oib_reason"].value_counts().items()}}
    return out


def entry_diagnostics(reg, manifest, window_name, params) -> list:
    cols = ["close", "oib_upper", "oib_lower", "oib_endpoint_ms", "oib_oi_current", "oib_oi_prior",
            "oib_oi_current_ms", "oib_oi_prior_ms", "oib_oi_current_age_ms", "oib_oi_prior_age_ms",
            "oib_oi_change", "oib_threshold", "oib_coverage", "oib_max_gap_ms", "oib_reason",
            "oib_source", "oib_time_basis", "oib_window_sha256", "signal"]
    rows = []
    for ds in manifest["datasets"]:
        _, win, cand = candidate_frame(reg, manifest, ds, window_name, params)
        scored = om.slice_window(cand, win)
        for ts, r in scored[scored["signal"] != 0][cols].iterrows():
            entry = {"dataset": ds["key"], "bar_open": str(ts)}
            for c in cols:
                v = r[c]
                entry[c] = v.item() if hasattr(v, "item") else v
            rows.append(entry)
    return rows


def matched_leg(reg, spec, manifest, dataset, window_name, params, cost) -> dict:
    from atr import ensure_atr_indicator
    from backtester import Backtester
    frame, win, cand = candidate_frame(reg, manifest, dataset, window_name, params)
    frame, funding_cov = om.attach_funding_cost(frame, dataset, win)
    base = reg.apply_strategy(BASELINE, frame, {"entry_period": params["price_lookback"]})
    signal = base["signal"].to_numpy().copy()
    signal[~cand["oib_oi_valid"].to_numpy(dtype=bool)] = 0
    base["signal"] = signal
    base = ensure_atr_indicator(base)
    scored = om.slice_window(base, win)
    ex = spec["exit_and_risk"]
    bt = Backtester(initial_capital=ex["capital_usd"], platform="hyperliquid",
                    open_strategy={"name": MATCHED, "params": {"entry_period": params["price_lookback"]}},
                    close_strategies=[ex["close_strategy"]], direction=ex["direction"],
                    stop_loss_atr_mult=1.0, comparison_mode=COMPARISON_MODE,
                    execution_spec=om.execution_spec(manifest, dataset, cost))
    results = bt.run(scored, strategy_name=MATCHED, symbol=dataset["symbol"], timeframe=manifest["interval"],
                     params={"entry_period": params["price_lookback"]}, save=False, indicator_frame=base)
    closes = om.slice_window(frame, win)["close"].astype(float)
    leg = leg_from_results(results, bh_return_pct=round((closes.iloc[-1] - closes.iloc[0]) / closes.iloc[0] * 100, 2))
    leg["execution"] = execution_metrics(results, ex["capital_usd"])
    leg["manifest"] = {"funding_coverage": funding_cov}
    leg["close_validation"] = results.get("close_validation")
    return leg


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
        "close_validation": aggregate_close_validations(
            leg.get("close_validation") for leg in present.values()),
    }


def _legs_from_score(score: dict) -> dict:
    return {r["dataset"]: r["leg"] for r in score["rows"]}


def select(rows: list, n_datasets: int) -> dict:
    eligible = [(i, r) for i, r in enumerate(rows)
                if r["summary"]["datasets"] == n_datasets and r["summary"]["positions"] >= MIN_TRAIN_POSITIONS]
    if not eligible:
        return {"rule_outcome": "no_eligible_combo", "selected": None}
    best = max(eligible, key=lambda kv: (kv[1]["summary"]["mean_ddadj"], kv[1]["summary"]["mean_sharpe"], -kv[0]))[1]
    return {"rule_outcome": "selected", "selected": best["label"], "params": best["params"]}


def m1_coverage(manifest: dict) -> dict:
    out = {}
    for name, (start, end) in WINDOWS.items():
        start_ms = om._ts_ms(start)
        end_ms = om._ts_ms(end) if end else om._ts_ms(manifest["windows"][HELD_OUT_WINDOW]["end"])
        per = {}
        for ds in manifest["datasets"]:
            per[ds["key"]] = om.open_interest_coverage(om.load_open_interest(ds), start_ms, end_ms)
        usable = all((c.get("bucket_coverage") or 0) >= MIN_OI_VALID_SHARE for c in per.values())
        out[name] = {"range": [start, end], "status": "available" if usable else "unavailable",
                     "datasets": per}
    return out


def verdict(cand_base, cand_stress, base, matched, oi_share) -> dict:
    checks = {
        "sample_sufficient": cand_base["positions"] >= MIN_TEST_POSITIONS,
        "open_interest_coverage": all(v["share"] >= MIN_OI_VALID_SHARE for v in oi_share.values()),
        "net_positive_base": cand_base.get("net_pnl_usd", 0) > 0,
        "net_positive_stress": cand_stress.get("net_pnl_usd", 0) > 0,
        "beats_donchian_ddadj": cand_base.get("mean_ddadj", 0) > base.get("mean_ddadj", 0),
        "beats_coverage_matched_ddadj": cand_base.get("mean_ddadj", 0) > matched.get("mean_ddadj", 0),
        "not_concentrated": (cand_base.get("top_dataset_abs_pnl_share") or 1.0) <= MAX_COIN_PNL_SHARE,
    }
    if not checks["sample_sufficient"] or not checks["open_interest_coverage"]:
        missing = []
        if not checks["sample_sufficient"]:
            missing.append(f"{cand_base['positions']} held-out positions < {MIN_TEST_POSITIONS}")
        if not checks["open_interest_coverage"]:
            low = sorted(k for k, v in oi_share.items() if v["share"] < MIN_OI_VALID_SHARE)
            missing.append(f"open-interest-valid bar share below {MIN_OI_VALID_SHARE} on {', '.join(low)}")
        outcome, reason = "inconclusive", "; ".join(missing)
    elif all(checks.values()):
        outcome, reason = "pass", "every frozen held-out check passed"
    else:
        outcome, reason = "fail", "failed frozen checks: " + ", ".join(sorted(k for k, v in checks.items() if not v))
    return {"outcome": outcome, "reason": reason, "checks": checks,
            "live_status": "research-only; backtest_only=True regardless of outcome"}


def run(manifest_path: str) -> dict:
    from registry_loader import load_registry
    spec = _spec()
    manifest = om.load_manifest(manifest_path)
    recording = verify_recording(manifest)
    reg = load_registry("futures")
    datasets = manifest_datasets(manifest)
    memo: dict = {}

    def score(name, params, window, cost):
        return evaluate_window(reg, arm(spec, name, params), datasets, window, spec["exit_and_risk"]["capital_usd"],
                               memo, manifest=manifest, cost_multiplier=cost)

    defaults = dict(reg.STRATEGY_REGISTRY[CANDIDATE]["default_params"])
    train_rows = []
    for label, params in _grid(defaults):
        s = score(CANDIDATE, params, SELECTION_WINDOW, BASE_COST)
        summary = summarize(_legs_from_score(s))
        summary.pop("per_dataset", None)
        train_rows.append({"label": label, "params": params, "summary": summary})
    choice = select(train_rows, len(datasets))
    selected = choice["params"] if choice["selected"] else defaults
    base_params = {"entry_period": selected["price_lookback"]}

    held = {}
    cand_score = score(CANDIDATE, selected, HELD_OUT_WINDOW, BASE_COST)
    held[CANDIDATE] = {"params": selected,
                       "base": summarize(_legs_from_score(cand_score)),
                       "stress": summarize(_legs_from_score(score(CANDIDATE, selected, HELD_OUT_WINDOW, STRESS_COST))),
                       "m1_incumbent_verdict": cand_score.get("verdict"),
                       "m1_incumbent_bar_mean_ddadj": cand_score.get("mean_bar_ddadj")}
    held[BASELINE] = {"params": base_params,
                      "base": summarize(_legs_from_score(score(BASELINE, base_params, HELD_OUT_WINDOW, BASE_COST))),
                      "stress": summarize(_legs_from_score(score(BASELINE, base_params, HELD_OUT_WINDOW, STRESS_COST)))}
    held[MATCHED] = {"params": base_params, "mask_params": selected,
                     "base": summarize({ds["key"]: matched_leg(reg, spec, manifest, ds, HELD_OUT_WINDOW, selected, BASE_COST)
                                        for ds in manifest["datasets"]}),
                     "stress": summarize({ds["key"]: matched_leg(reg, spec, manifest, ds, HELD_OUT_WINDOW, selected, STRESS_COST)
                                          for ds in manifest["datasets"]})}
    oi_share = {w: oi_valid_share(reg, manifest, w, selected) for w in (SELECTION_WINDOW, HELD_OUT_WINDOW)}

    variants = []
    for row in train_rows:
        test_summary = summarize(_legs_from_score(score(CANDIDATE, row["params"], HELD_OUT_WINDOW, BASE_COST)))
        variants.append({"label": row["label"], "params": row["params"],
                         "train_positions": row["summary"]["positions"],
                         "train_mean_ddadj": row["summary"].get("mean_ddadj"),
                         "test_positions": test_summary["positions"],
                         "test_mean_ddadj": test_summary.get("mean_ddadj"),
                         "test_net_pnl_usd": test_summary.get("net_pnl_usd")})

    result = {
        "study": manifest["study"],
        "issue": 1637,
        "spec_sha256": _sha(SPEC_PATH),
        "manifest": {
            "path": os.path.relpath(manifest["path"], _REPO),
            "sha256": _sha(manifest["path"]),
            "schema": manifest["schema"],
            "provenance": manifest["provenance"],
            "interval": manifest["interval"],
            "warmup_bars": manifest["warmup_bars"],
            "windows": {k: {"start": v["start"], "end": v["end"], "role": v["role"]}
                        for k, v in manifest["windows"].items()},
            "costs": manifest["costs"],
            "datasets": [{"key": d["key"], "size_decimals": d["size_decimals"], "half_spread_bps": d["half_spread_bps"],
                          "candles_sha256": d["candles"]["sha256"],
                          "funding_sha256": d["funding"]["sha256"] if d["funding"] else None,
                          "open_interest_sha256": d["open_interest"]["sha256"]}
                         for d in manifest["datasets"]],
            "venue_reference": {"meta_sha256": manifest["venue_reference"]["meta"]["sha256"],
                                "l2_snapshot_sha256": manifest["venue_reference"].get("l2_snapshot", {}).get("sha256"),
                                "documents": manifest["venue_reference"]["documents"]},
            "recording": manifest["raw"].get("recording"),
        },
        "recording": recording,
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
            "incumbents_reference": list(INCUMBENTS),
        },
        "selection": {"grid": {k: list(v) for k, v in DEFAULT_PARAM_RANGES[CANDIDATE].items()},
                      "train_sweep": train_rows, "choice": choice, "selected_params": selected},
        "open_interest_valid_share": oi_share,
        "held_out": held,
        "held_out_entries": entry_diagnostics(reg, manifest, HELD_OUT_WINDOW, selected),
        "train_entries": entry_diagnostics(reg, manifest, SELECTION_WINDOW, selected),
        "variants": variants,
        "m1_windows": m1_coverage(manifest),
    }
    result["verdict"] = verdict(held[CANDIDATE]["base"], held[CANDIDATE]["stress"], held[BASELINE]["base"],
                                held[MATCHED]["base"], oi_share[HELD_OUT_WINDOW])
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


def render(result: dict) -> str:
    v = result["verdict"]
    m = result["manifest"]
    lines = [
        "# Open-interest breakout: recorded held-out result (#1637)",
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
                     f"`{d['candles_sha256'][:16]}`, funding `{(d['funding_sha256'] or '')[:16]}`, open interest "
                     f"`{d['open_interest_sha256'][:16]}`")
    r = result["recording"]
    lines += [
        f"- Recording run `{r['run_id']}`: {len(r['segments'])} hashed segments, {r['accepted']} accepted bucket samples, "
        f"{r['refreshed']} same-bucket refreshes, {r['rejected']} rejected records, {r['dropped']} dropped records, "
        f"sessions {r['sessions']}",
        "- Time basis: local receipt time. The venue sends no event timestamp, so receipt-time parity with live "
        "decisions holds by construction and no archive timestamp is used.",
        "",
        "## Protocol",
        "",
        f"- Every arm: direction `{result['protocol']['direction']}`, close `time_stop` "
        f"(max_bars {result['protocol']['close_strategy']['params']['max_bars']}), {result['protocol']['stop']}, "
        f"capital ${result['protocol']['capital_usd']:.0f}; {result['protocol']['sizing']}.",
        f"- Close comparison mode: `{result['protocol'].get('comparison_mode') or 'not recorded (pre-#1683 run)'}`. "
        "time_stop reads the simulator's held-bar count, which no live close context supplies, so every arm here is "
        "research evidence with incomplete close parity, never strict live parity proof.",
        f"- Selection: {result['protocol']['selection_rule']}",
        f"- Selection outcome: {result['selection']['choice']['rule_outcome']}; scored params "
        f"`{json.dumps(result['selection']['selected_params'], sort_keys=True)}`",
        "",
        "## Open-interest coverage (selected params)",
        "",
        "| Window | Dataset | Scored bars | OI-valid bars | Share | Reasons |",
        "|---|---|---|---|---|---|",
    ]
    for w, per in result["open_interest_valid_share"].items():
        for ds, s in sorted(per.items()):
            reasons = ", ".join(f"{k} {n}" for k, n in sorted(s["reasons"].items()))
            lines.append(f"| {w} | {ds} | {s['bars']} | {s['oi_valid_bars']} | {_fmt(s['share'], 3)} | {reasons} |")
    lines += ["", "## Held-out comparison", "",
              "| Arm | Cost | Positions (L/S) | Net $ | Fees $ | Funding $ | Mean ret % | Mean DDadj | Worst DD % | Turnover | Top coin share | Overlap share |",
              "|---|---|---|---|---|---|---|---|---|---|---|---|"]
    for name, h in result["held_out"].items():
        for label in ("base", "stress"):
            s = h[label]
            if not s.get("datasets"):
                lines.append(f"| `{name}` | {label} | 0 | - | - | - | - | - | - | - | - | - |")
                continue
            lines.append(f"| `{name}` | {label} | {s['positions']} ({s['long_positions']}/{s['short_positions']}) | "
                         f"{_fmt(s['net_pnl_usd'])} | {_fmt(s['fees_usd'])} | {_fmt(s['funding_pnl_usd'], 4)} | "
                         f"{_fmt(s['mean_return_pct'], 3)} | {_fmt(s['mean_ddadj'], 3)} | {_fmt(s['worst_max_dd_pct'], 3)} | "
                         f"{_fmt(s['mean_turnover'])} | {_fmt(s['top_dataset_abs_pnl_share'])} | "
                         f"{_fmt(s['same_side_cross_asset_overlap_share'])} |")
    lines += ["", "## Held-out entries (candidate, selected params)", ""]
    if not result["held_out_entries"]:
        lines.append("No candidate entry on the held-out window.")
    else:
        lines += ["| Dataset | Bar open | Side | Close | Channel | OI prior -> current | Change | Ages ms | Coverage | Max gap ms |",
                  "|---|---|---|---|---|---|---|---|---|---|"]
        for e in result["held_out_entries"]:
            side = "long" if e["signal"] > 0 else "short"
            lines.append(f"| {e['dataset']} | {e['bar_open']} | {side} | {_fmt(e['close'], 4)} | "
                         f"[{_fmt(e['oib_lower'], 4)}, {_fmt(e['oib_upper'], 4)}] | {_fmt(e['oib_oi_prior'], 4)} -> "
                         f"{_fmt(e['oib_oi_current'], 4)} | {_fmt(e['oib_oi_change'], 5)} | "
                         f"{_fmt(e['oib_oi_prior_age_ms'], 0)} / {_fmt(e['oib_oi_current_age_ms'], 0)} | "
                         f"{_fmt(e['oib_coverage'], 3)} | {_fmt(e['oib_max_gap_ms'], 0)} |")
    lines += ["", "## Verdict checks", ""]
    for k, ok in sorted(v["checks"].items()):
        lines.append(f"- {k}: {_fmt(ok)}")
    lines += ["", "## M1 windows", "",
              "| Window | Range | Status | Bucket coverage (BTC/ETH/SOL) |", "|---|---|---|---|"]
    for name, w in result["m1_windows"].items():
        covs = "/".join(_fmt(c.get("bucket_coverage"), 5) for _, c in sorted(w["datasets"].items()))
        lines.append(f"| `{name}` | {w['range'][0]} to {w['range'][1] or 'open'} | {w['status']} | {covs} |")
    lines += ["", "## All grid variants (train selection, held-out outcome)", "",
              "| Variant | Train positions | Train DDadj | Held-out positions | Held-out DDadj | Held-out net $ |",
              "|---|---|---|---|---|---|"]
    for row in result["variants"]:
        lines.append(f"| {row['label']} | {row['train_positions']} | {_fmt(row['train_mean_ddadj'], 3)} | "
                     f"{row['test_positions']} | {_fmt(row['test_mean_ddadj'], 3)} | {_fmt(row['test_net_pnl_usd'])} |")
    lines += ["", "## Source hashes", ""]
    for path, digest in sorted(result["source_sha256"].items()):
        lines.append(f"- `{path}` `{digest}`")
    lines.append("")
    return "\n".join(lines)


def main(argv=None) -> int:
    p = argparse.ArgumentParser(description="#1637 recorded open-interest study driver (research-only)")
    sub = p.add_subparsers(dest="cmd")
    prep = sub.add_parser("prepare", help="Freeze a closed recording run into data/ and pin its series hashes")
    prep.add_argument("--run-dir", required=True)
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
    except (om.ManifestError, orp.RecordingError) as exc:
        print(f"study error: {exc}", file=sys.stderr)
        return 1
    with open(report, "w") as fh:
        fh.write(render(result))
    print(f"verdict: {result['verdict']['outcome']} ({result['verdict']['reason']})")
    print(f"wrote {json_out} and {report}")
    return 0


if __name__ == "__main__":
    sys.exit(main())

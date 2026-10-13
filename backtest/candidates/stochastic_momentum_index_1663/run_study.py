#!/usr/bin/env python3

import argparse
import concurrent.futures
import gzip
import hashlib
import json
import math
import os
import sqlite3
import statistics
import sys
import tempfile

import numpy as np

_HERE = os.path.dirname(os.path.abspath(__file__))
_BACKTEST = os.path.abspath(os.path.join(_HERE, "..", ".."))
_REPO = os.path.abspath(os.path.join(_BACKTEST, ".."))
for _p in (_BACKTEST, os.path.join(_REPO, "shared_tools")):
    if _p not in sys.path:
        sys.path.insert(0, _p)

CACHE_ENV = "GO_TRADER_OHLCV_CACHE_DB"
SEED_PATH = os.path.join(_HERE, "candidate_seed.json")
MANIFEST = os.path.join(_REPO, "backtest", "candidates", "stochastic_momentum_index_1663_manifest.json")
PROXY_DIR = os.path.join(_REPO, "backtest", "candidates", "ultimate_oscillator_1664", "data")
EVIDENCE_DIR = os.path.join(_HERE, "evidence")
RESULTS_PATH = os.path.join(_HERE, "results.json")
REPORT_PATH = os.path.join(_HERE, "REPORT.md")
PARITY_PATH = os.path.join(_HERE, "parity_results.json")
HOUR_MS = 3_600_000
STEP_MS = 14_400_000
CANDLE_EXCHANGE = "binanceus"
FUNDING_EXCHANGE = "hyperliquid"
SOURCE_FILES = (
    "shared_strategies/open/stochastic_momentum_index.py",
    "shared_strategies/open/registry.py",
    "shared_strategies/close/tiered_tp_pct.py",
    "shared_tools/closed_bar.py",
    "shared_tools/strategy_composition.py",
    "shared_tools/funding_fetcher.py",
    "shared_tools/storage.py",
    "shared_tools/data_fetcher.py",
    "shared_scripts/check_hyperliquid.py",
    "backtest/backtester.py",
    "backtest/eval_windows.py",
    "backtest/offline_manifest.py",
    "backtest/optimizer.py",
    "backtest/parity_diff.py",
    "backtest/candidates/stochastic_momentum_index_1663/candidate_seed.json",
    "backtest/candidates/stochastic_momentum_index_1663/run_study.py",
    "backtest/candidates/stochastic_momentum_index_1663/run_parity.py",
)
EVIDENCE_INPUT_COLUMNS = ("timestamp", "open", "high", "low", "close", "volume")
EVIDENCE_VALUE_COLUMNS = (
    "smi_eval_ts_ms", "smi_seed_relative", "smi_seed_range", "smi_relative_smoothed", "smi_range_smoothed",
    "smi_value", "smi_signal_line", "smi_diff", "smi_prev_value", "smi_prev_signal_line", "smi_prev_diff",
    "smi_value_valid", "smi_prev_valid", "smi_valid", "smi_reason_code",
)


def load_seed(path: str = SEED_PATH) -> dict:
    with open(path) as fh:
        return json.load(fh)


SEED = load_seed()
CANDIDATE = SEED["name"]
ARMS = tuple(SEED["arms"])
CANDIDATE_ARMS = tuple(SEED["candidate_arms"])
BASELINE_ARMS = tuple(SEED["baseline_arms"])
SELECTION_WINDOW = SEED["selection_window"]
HELD_OUT_WINDOW = SEED["held_out_window"]
WINDOWS = {name: (spec[0], spec[1]) for name, spec in SEED["windows"].items()}
COIN_DATASETS = tuple((key.split()[0], key.split()[1]) for key in SEED["datasets"])
WARMUP = SEED["warmup_bars"]
CAPITAL = SEED["capital"]
CLOSE = SEED["close_strategies"]
STOP = SEED["stop_loss_atr_mult"]
INTRABAR = SEED["intrabar_resolution"]
FUNDING_MODE = SEED["funding_mode"]
MIN_TRAIN_POSITIONS = SEED["selection"]["min_train_independent_positions"]
MIN_TEST_POSITIONS = SEED["held_out"]["min_independent_positions_per_arm"]
MIN_SIDE_POSITIONS = SEED["held_out"]["min_positions_per_direction_bidirectional"]
VENUE_BASE = SEED["cost_runs"]["venue_base"]["cost_multiplier"]
VENUE_STRESS = SEED["cost_runs"]["venue_stress"]["cost_multiplier"]
PROXY_BASE = SEED["cost_runs"]["proxy_base"]["slippage_mult"]
PROXY_STRESS = SEED["cost_runs"]["proxy_stress"]["slippage_mult"]
MAX_MISSING_SHARE = SEED["proxy_inputs"]["max_missing_bar_share"]
STABILITY_SHARE = 2.0 / 3.0
PROVENANCES = ("venue", "proxy")
LIVE_STATUS = ("edge_status=no_edge regardless of outcome; paper needs exactly one explicit --mode=paper, "
               "live or missing mode needs allow_no_edge: true; no acknowledgement, promotion or deployment")


def sha256_file(path: str) -> str:
    h = hashlib.sha256()
    with open(path, "rb") as fh:
        for chunk in iter(lambda: fh.read(1 << 20), b""):
            h.update(chunk)
    return h.hexdigest()


def protocol_sha256() -> str:
    blob = json.dumps(SEED, sort_keys=True, separators=(",", ":")).encode("utf-8")
    return hashlib.sha256(blob).hexdigest()


def _ms(day: str) -> int:
    import pandas as pd
    return int(pd.Timestamp(day, tz="UTC").value // 1_000_000)


def arm_strategy(arm: str) -> str:
    return SEED["arms"][arm]["strategy"]


def arm_direction(arm: str) -> str:
    return SEED["arms"][arm]["direction"]


def proxy_inventory() -> dict:
    with open(os.path.join(PROXY_DIR, "frozen_inputs.json")) as fh:
        inventory = json.load(fh)
    by_path = {f["path"]: f for f in inventory["files"]}
    files = []
    for name in SEED["proxy_inputs"]["files"]:
        entry = by_path.get(name)
        if entry is None:
            raise SystemExit(f"proxy input {name} is not listed in the frozen inventory")
        actual = sha256_file(os.path.join(PROXY_DIR, name))
        if actual != entry["sha256"]:
            raise SystemExit(f"proxy input {name} sha256 {actual} != pinned {entry['sha256']}")
        files.append(entry)
    return {"inventory_path": os.path.relpath(os.path.join(PROXY_DIR, "frozen_inputs.json"), _REPO),
            "inventory_sha256": sha256_file(os.path.join(PROXY_DIR, "frozen_inputs.json")),
            "candle_source": inventory.get("candle_source"), "funding_source": inventory.get("funding_source"),
            "files": files}


def _gz_lines(path: str) -> list:
    with gzip.open(path, "rt") as fh:
        return fh.read().splitlines()


def build_proxy_cache(inventory: dict, db_path: str) -> None:
    import storage
    storage.init_db(db_path)
    conn = sqlite3.connect(db_path)
    try:
        for f in inventory["files"]:
            path = os.path.join(PROXY_DIR, f["path"])
            if f["kind"] == "candles":
                rows = []
                for line in _gz_lines(path)[1:]:
                    t, o, h, lo, c, v = line.split(",")
                    rows.append((CANDLE_EXCHANGE, f["symbol"], f["timeframe"], int(t), float(o), float(h),
                                 float(lo), float(c), float(v)))
                conn.executemany(
                    "INSERT INTO ohlcv (exchange, symbol, timeframe, timestamp, open, high, low, close, volume)"
                    " VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)", rows)
            elif f["kind"] == "funding":
                conn.executemany(
                    "INSERT INTO funding_rates (exchange, coin, timestamp, rate) VALUES (?, ?, ?, ?)",
                    [(FUNDING_EXCHANGE, f["coin"], int(t), float(r))
                     for t, r in (line.split(",") for line in _gz_lines(path)[1:])])
            elif f["kind"] == "funding_meta":
                with open(path) as fh:
                    meta = json.load(fh)
                conn.executemany(
                    "INSERT INTO funding_coverage (exchange, coin, start_ts, end_ts) VALUES (?, ?, ?, ?)",
                    [(FUNDING_EXCHANGE, f["coin"], int(s), int(e)) for s, e in meta["coverage"]])
                conn.executemany(
                    "INSERT INTO funding_venue_gaps (exchange, coin, timestamp, observed_through_ms, recorded_at_ms)"
                    " VALUES (?, ?, ?, ?, ?)",
                    [(FUNDING_EXCHANGE, f["coin"], int(t), int(o), int(r)) for t, o, r in meta["venue_gaps"]])
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


_WORKER = {}


def _worker_init(db_path: str) -> None:
    os.environ[CACHE_ENV] = db_path
    import storage
    if os.path.abspath(storage.DB_PATH) != os.path.abspath(db_path):
        raise RuntimeError(f"worker cache {storage.DB_PATH} is not the private study cache {db_path}")
    forbid_network()


def _worker_state() -> dict:
    if not _WORKER:
        import offline_manifest as om
        from registry_loader import load_registry
        _WORKER["reg"] = load_registry("futures")
        _WORKER["manifest"] = om.load_manifest(MANIFEST)
    return _WORKER


def _leg_record(leg, symbol: str, prov: str) -> dict:
    ds = f"{symbol} 4h"
    if leg is None:
        return {"dataset": ds, "missing": True}
    ex = leg.get("execution") or {}
    funding = leg.get("funding") or {}
    rec = {
        "dataset": ds,
        "sharpe": leg["sharpe"],
        "return_pct": leg["return_pct"],
        "max_dd_pct": leg["max_dd_pct"],
        "ddadj": leg["ddadj"],
        "liquidated": bool(leg.get("liquidated")),
        "net_pnl_usd": ex.get("net_pnl_usd"),
        "fees_usd": ex.get("fees_usd"),
        "funding_pnl_usd": ex.get("funding_pnl_usd"),
        "turnover": ex.get("turnover"),
        "bars_in_market": ex.get("bars_in_market"),
        "rejected_entries": ex.get("rejected_entries", 0),
        "skipped_partial_closes": ex.get("skipped_partial_closes", 0),
        "funding_incomplete": bool(leg.get("funding_incomplete")),
        "funding_mode": funding.get("mode") if prov == "proxy" else "manifest",
        "funding_complete": bool(funding.get("complete")) if prov == "proxy" else None,
        "close_validation": leg.get("close_validation"),
        "positions": [{"entry": p["entry_date"], "exit": p["exit_date"], "side": p["side"],
                       "net_pnl": p["net_pnl"], "exit_reasons": sorted({(r or "").split(":")[0] or "unknown"
                                                                        for r in p["exit_reasons"]})}
                      for p in ex.get("position_list") or []],
    }
    if prov == "venue":
        man = leg["manifest"]
        rec["candle_coverage_complete"] = bool(man["candle_coverage"]["complete"])
        rec["funding_coverage_complete"] = bool(man["funding_coverage"]["complete"])
        rec["cost_model"] = man["cost_model"]
        rec["cost_multiplier"] = man["cost_multiplier"]
    else:
        rec["cost_model"] = "proxy_flat"
    return rec


def _task(task: tuple) -> dict:
    kind = task[0]
    st = _worker_state()
    from eval_windows import compute_incumbent_legs, incumbent_bars, run_leg
    if kind == "incumbents":
        _, prov, window, cost = task
        contract = {"direction": SEED["incumbent_reference"]["contract"]["direction"],
                    "close_strategies": json.loads(json.dumps(CLOSE)), "stop_loss_atr_mult": STOP}
        datasets = list(COIN_DATASETS)
        if prov == "venue":
            ctx = {"manifest": st["manifest"], "window": window, "cost_multiplier": cost}
            legs = compute_incumbent_legs(st["reg"], datasets, WINDOWS[window], CAPITAL, intrabar_resolution=INTRABAR,
                                          manifest_ctx=ctx, funding_mode=FUNDING_MODE, contract=contract)
        else:
            legs = compute_incumbent_legs(st["reg"], [(f"{c}/USDT", tf) for c, tf in datasets], WINDOWS[window],
                                          CAPITAL, intrabar_resolution=INTRABAR, funding_mode=FUNDING_MODE,
                                          contract=contract, warmup_bars=WARMUP)
            legs = {k.replace("/USDT", ""): v for k, v in legs.items()}
        bars = incumbent_bars(legs)
        members = {ds: {name: (None if leg is None else {"sharpe": leg["sharpe"], "ddadj": leg["ddadj"],
                                                          "funding_incomplete": bool(leg.get("funding_incomplete")),
                                                          "liquidated": bool(leg.get("liquidated"))})
                        for name, leg in per.items()} for ds, per in legs.items()}
        return {"bars": bars, "members": members}
    _, prov, arm, params, window, cost = task
    from backtester import DEFAULT_SLIPPAGE_PCT
    out = []
    for coin, tf in COIN_DATASETS:
        kwargs = dict(capital=CAPITAL, close_strategies=json.loads(json.dumps(CLOSE)), direction=arm_direction(arm),
                      stop_loss_atr_mult=STOP, intrabar_resolution=INTRABAR, funding_mode=FUNDING_MODE)
        if prov == "venue":
            symbol = coin
            leg = run_leg(st["reg"], arm_strategy(arm), dict(params), symbol, tf, WINDOWS[window],
                          manifest_ctx={"manifest": st["manifest"], "window": window, "cost_multiplier": cost},
                          **kwargs)
        else:
            symbol = f"{coin}/USDT"
            leg = run_leg(st["reg"], arm_strategy(arm), dict(params), symbol, tf, WINDOWS[window],
                          slippage_pct=DEFAULT_SLIPPAGE_PCT * cost, keep_trades=True, warmup_bars=WARMUP,
                          **kwargs)
        out.append(_leg_record(leg, coin, prov))
    return {"legs": out}


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

    def run(self, tasks: list) -> list:
        keys = [json.dumps(t, sort_keys=True, default=str) for t in tasks]
        todo = []
        seen = set()
        for k, t in zip(keys, tasks):
            if k not in self.cache and k not in seen:
                seen.add(k)
                todo.append((k, t))
        if todo:
            if self.workers > 1:
                if self.pool is None:
                    self.pool = concurrent.futures.ProcessPoolExecutor(
                        max_workers=self.workers, initializer=_worker_init, initargs=(self.db_path,))
                for (k, _), res in zip(todo, self.pool.map(_task, [t for _, t in todo], chunksize=2)):
                    self.cache[k] = res
            else:
                for k, t in todo:
                    self.cache[k] = _task(t)
        return [self.cache[k] for k in keys]


def leg_task(prov, arm, params, window, cost):
    return ("arm", prov, arm, dict(params), window, float(cost))


def independent_positions(positions: list) -> int:
    import pandas as pd
    spans = sorted((pd.Timestamp(p["entry"]), pd.Timestamp(p["exit"])) for p in positions)
    count = 0
    latest_exit = None
    for entry, exit_ in spans:
        if latest_exit is None or entry > latest_exit:
            count += 1
            latest_exit = exit_
        else:
            latest_exit = max(latest_exit, exit_)
    return count


def leg_complete(rec: dict, prov: str, coverage: dict, window: str) -> bool:
    if rec.get("missing"):
        return False
    if rec["funding_incomplete"]:
        return False
    if prov == "venue":
        return rec["candle_coverage_complete"] and rec["funding_coverage_complete"]
    if rec.get("funding_mode") != FUNDING_MODE:
        return False
    cov = coverage.get(f"{rec['dataset']}|{window}") or {}
    return bool(cov.get("complete")) and bool(rec.get("funding_complete"))


def summarize(result: dict, prov: str, window: str, coverage: dict, window_bars: dict, keep_detail=True) -> dict:
    recs = result["legs"]
    present = [r for r in recs if not r.get("missing")]
    out = {"datasets": len(present), "expected_datasets": len(COIN_DATASETS)}
    if not present:
        out.update({"positions": 0, "independent_positions": 0, "long_positions": 0, "short_positions": 0,
                    "coverage_complete": False, "liquidated_legs": 0})
        return out
    positions = [dict(p, dataset=r["dataset"]) for r in present for p in r["positions"]]
    abs_ds = [abs(r["net_pnl_usd"]) for r in present]
    abs_pos = sorted((abs(p["net_pnl"]) for p in positions), reverse=True)
    exit_reasons = {}
    for p in positions:
        for reason in p["exit_reasons"]:
            exit_reasons[reason] = exit_reasons.get(reason, 0) + 1
    out.update({
        "mean_return_pct": round(statistics.mean(r["return_pct"] for r in present), 4),
        "mean_sharpe": round(statistics.mean(r["sharpe"] for r in present), 4),
        "mean_ddadj": round(statistics.mean(r["ddadj"] for r in present), 4),
        "worst_max_dd_pct": round(max(abs(r["max_dd_pct"]) for r in present), 4),
        "net_pnl_usd": round(sum(r["net_pnl_usd"] for r in present), 4),
        "fees_usd": round(sum(r["fees_usd"] for r in present), 4),
        "funding_pnl_usd": round(sum(r["funding_pnl_usd"] for r in present), 4),
        "mean_turnover": round(statistics.mean(r["turnover"] for r in present), 4),
        "positions": len(positions),
        "long_positions": sum(1 for p in positions if p["side"] == "long"),
        "short_positions": sum(1 for p in positions if p["side"] == "short"),
        "long_net_pnl_usd": round(sum(p["net_pnl"] for p in positions if p["side"] == "long"), 4),
        "short_net_pnl_usd": round(sum(p["net_pnl"] for p in positions if p["side"] == "short"), 4),
        "independent_positions": independent_positions(positions),
        "exit_reasons": dict(sorted(exit_reasons.items())),
        "liquidated_legs": sum(1 for r in present if r["liquidated"]),
        "top_dataset_abs_pnl_share": round(max(abs_ds) / sum(abs_ds), 4) if sum(abs_ds) > 0 else None,
        "top5_position_abs_pnl_share": round(sum(abs_pos[:5]) / sum(abs_pos), 4) if sum(abs_pos) > 0 else None,
        "coverage_complete": (len(present) == len(COIN_DATASETS)
                              and all(leg_complete(r, prov, coverage, window) for r in present)),
        "incomplete_legs": sorted(r["dataset"] for r in recs if not leg_complete(r, prov, coverage, window)),
    })
    if keep_detail:
        out["per_dataset"] = {
            r["dataset"]: {k: r[k] for k in ("return_pct", "sharpe", "max_dd_pct", "ddadj", "liquidated",
                                             "net_pnl_usd", "fees_usd", "funding_pnl_usd", "turnover",
                                             "rejected_entries", "skipped_partial_closes", "cost_model",
                                             "funding_incomplete")}
            | {"positions": len(r["positions"]),
               "long_positions": sum(1 for p in r["positions"] if p["side"] == "long"),
               "short_positions": sum(1 for p in r["positions"] if p["side"] == "short"),
               "time_in_market": round(r["bars_in_market"] / window_bars[f"{r['dataset']}|{window}"], 4)}
            for r in present}
        from backtester import aggregate_close_validations
        out["close_validation"] = aggregate_close_validations([r["close_validation"] for r in present])
    return out


def eligible(summary: dict) -> bool:
    return (summary["datasets"] == summary["expected_datasets"]
            and summary.get("coverage_complete", False)
            and summary.get("liquidated_legs", 1) == 0
            and summary["independent_positions"] >= MIN_TRAIN_POSITIONS)


def select(rows: list) -> dict:
    pool = [(i, r) for i, r in enumerate(rows) if eligible(r["summary"])]
    if not pool:
        return {"rule_outcome": "no_eligible_combo", "selected": None, "params": None, "eligible_combos": 0}
    best = max(pool, key=lambda kv: (kv[1]["summary"]["mean_sharpe"], kv[1]["summary"]["mean_ddadj"], -kv[0]))[1]
    return {"rule_outcome": "selected", "selected": best["label"], "params": best["params"],
            "eligible_combos": len(pool)}


def grid_rows(name: str, defaults: dict) -> list:
    from eval_windows import expand_sweep
    from optimizer import DEFAULT_PARAM_RANGES
    specs = [(k, list(v)) for k, v in DEFAULT_PARAM_RANGES[name].items()]
    return expand_sweep(dict(defaults), specs)


def neighbour_params(params: dict) -> list:
    from optimizer import DEFAULT_PARAM_RANGES
    ranges = DEFAULT_PARAM_RANGES[CANDIDATE]
    out = []
    for key, grid in ranges.items():
        grid = list(grid)
        i = grid.index(params[key])
        for j in (i - 1, i + 1):
            if 0 <= j < len(grid):
                out.append(dict(params, **{key: grid[j]}))
    return out


def venue_preflight(manifest: dict) -> dict:
    import offline_manifest as om
    rows = []
    for ds in manifest["datasets"]:
        for window_name in (SELECTION_WINDOW, HELD_OUT_WINDOW):
            out = {"dataset": ds["key"], "window": window_name, "problems": []}
            win = manifest["windows"][window_name]
            step = manifest["interval_ms"]
            try:
                frame, _, cov = om.window_frame(manifest, ds, window_name)
            except om.ManifestError as exc:
                out["problems"].append(f"candles: {exc}")
                out["complete"] = False
                rows.append(out)
                continue
            out["candle_coverage"] = cov
            ts = frame["timestamp"].to_numpy(dtype=np.int64)
            expected_rows = manifest["warmup_bars"] + cov["expected_bars"]
            if not cov["complete"] or cov["warmup_missing_bars"]:
                out["problems"].append(f"candles: {cov['missing_bars']} window bars and "
                                       f"{cov['warmup_missing_bars']} warm-up bars missing")
            if len(ts) != expected_rows or (len(ts) > 1 and not bool((np.diff(ts) == step).all())):
                out["problems"].append(f"candles: {len(ts)} rows off the exact grid (expected {expected_rows})")
            o, h, lo, c = (frame[k].to_numpy(dtype=float) for k in ("open", "high", "low", "close"))
            with np.errstate(invalid="ignore"):
                ok = (np.isfinite(o) & np.isfinite(h) & np.isfinite(lo) & np.isfinite(c) & (o > 0) & (h > 0)
                      & (lo > 0) & (c > 0) & (h >= lo) & (o <= h) & (o >= lo) & (c <= h) & (c >= lo))
            out["invalid_rows"] = int((~ok).sum())
            if out["invalid_rows"]:
                out["problems"].append(f"candles: {out['invalid_rows']} rows with invalid prices or geometry")
            if manifest["warmup_bars"] < 199:
                out["problems"].append(f"candles: warm-up {manifest['warmup_bars']} bars is below 199")
            funding = om.load_funding(ds)
            fcov = om.funding_coverage(funding, win["start_ms"], win["end_ms"])
            out["funding_coverage"] = fcov
            if not fcov["complete"]:
                out["problems"].append(f"funding: {fcov['missing_hours']} hourly records missing (left-closed)")
            first_slot = (win["start_ms"] - step) // HOUR_MS
            last_slot = (win["end_ms"] - step) // HOUR_MS
            have = set() if funding is None else set(
                (funding["timestamp"].to_numpy(dtype=np.int64) // HOUR_MS).tolist())
            missing_slots = [s for s in range(first_slot, last_slot + 1) if s not in have]
            out["accrual_span"] = {"first_slot_ms": first_slot * HOUR_MS, "last_slot_ms": last_slot * HOUR_MS,
                                   "expected_slots": last_slot - first_slot + 1,
                                   "missing_slots": len(missing_slots)}
            if missing_slots:
                out["problems"].append(f"funding: {len(missing_slots)} hourly records missing in the right-closed "
                                       "accrual span")
            out["complete"] = not out["problems"]
            rows.append(out)
    refused = [{"dataset": r["dataset"], "window": r["window"], "problems": r["problems"]}
               for r in rows if not r["complete"]]
    return {"complete": not refused, "refused": refused, "windows": rows}


def proxy_preflight(inventory: dict) -> dict:
    by_coin = {f["symbol"].split("/")[0]: f for f in inventory["files"] if f["kind"] == "candles"}
    meta = {}
    for f in inventory["files"]:
        if f["kind"] == "funding_meta":
            with open(os.path.join(PROXY_DIR, f["path"])) as fh:
                meta[f["coin"]] = json.load(fh)
    rows = []
    for coin, _ in COIN_DATASETS:
        ts = np.array([int(line.split(",")[0]) for line in _gz_lines(os.path.join(PROXY_DIR,
                                                                                    by_coin[coin]["path"]))[1:]])
        have = set(ts.tolist())
        for window_name in (SELECTION_WINDOW, HELD_OUT_WINDOW):
            start, end = (_ms(d) for d in WINDOWS[window_name])
            expected = (end - start) // STEP_MS
            missing = [t for t in range(start, end, STEP_MS) if t not in have]
            warm_missing = [t for t in range(start - WARMUP * STEP_MS, start, STEP_MS) if t not in have]
            share = len(missing) / expected
            problems = []
            if start not in have or end - STEP_MS not in have:
                problems.append("candles: first or last window bar missing")
            if warm_missing:
                problems.append(f"candles: {len(warm_missing)} of the {WARMUP} warm-up bars missing")
            if share > MAX_MISSING_SHARE:
                problems.append(f"candles: {len(missing)} missing bars exceed {MAX_MISSING_SHARE:.1%}")
            venue_gaps = [g for g in meta[coin]["venue_gaps"] if start - STEP_MS <= g[0] < end]
            rows.append({
                "dataset": f"{coin} 4h", "window": window_name, "expected_bars": int(expected),
                "present_bars": int(expected - len(missing)), "missing_bars": len(missing),
                "missing_bar_share": round(share, 6),
                "missing_bar_times": [str(np.datetime64(int(t), "ms")) for t in missing],
                "warmup_missing_bars": len(warm_missing),
                "funding_coverage_intervals": meta[coin]["coverage"],
                "funding_venue_gap_hours_in_span": [[str(np.datetime64(int(g[0]), "ms")), int(g[1]), int(g[2])]
                                                    for g in venue_gaps],
                "problems": problems, "complete": not problems,
            })
    refused = [{"dataset": r["dataset"], "window": r["window"], "problems": r["problems"]}
               for r in rows if not r["complete"]]
    return {"complete": not refused, "refused": refused, "windows": rows}


def spearman(a: list, b: list):
    if len(a) < 3:
        return None

    def rank(values):
        order = sorted(range(len(values)), key=lambda i: values[i])
        ranks = [0.0] * len(values)
        i = 0
        while i < len(order):
            j = i
            while j + 1 < len(order) and values[order[j + 1]] == values[order[i]]:
                j += 1
            for k in range(i, j + 1):
                ranks[order[k]] = (i + j) / 2.0
            i = j + 1
        return ranks

    ra, rb = rank(a), rank(b)
    ma, mb = statistics.mean(ra), statistics.mean(rb)
    num = sum((x - ma) * (y - mb) for x, y in zip(ra, rb))
    den = (sum((x - ma) ** 2 for x in ra) * sum((y - mb) ** 2 for y in rb)) ** 0.5
    return round(num / den, 4) if den else None


def stability(rows: list) -> dict:
    evaluable = [r for r in rows if r["coverage_complete"] and r["datasets"] == len(COIN_DATASETS)]
    positive = [r for r in evaluable if (r["net_pnl_usd"] or 0) > 0]
    share = len(positive) / len(evaluable) if evaluable else None
    return {"neighbours": len(rows), "evaluable": len(evaluable), "net_positive": len(positive),
            "net_positive_share": round(share, 4) if share is not None else None,
            "met": bool(evaluable) and share >= STABILITY_SHARE, "rows": rows}


def provenance_verdict(prov: str, gate: dict, selections: dict, held: dict, stab: dict) -> dict:
    if not gate["complete"]:
        return {"outcome": "inconclusive", "checks": {"coverage_complete": False},
                "reason": "coverage gate refused " + ", ".join(f"{r['dataset']} {r['window']}"
                                                               for r in gate["refused"])}
    unselected = [a for a in ARMS if selections[a] is None]
    if unselected:
        return {"outcome": "inconclusive", "checks": {"coverage_complete": True, "every_arm_selected": False},
                "reason": "no eligible training selection for " + ", ".join(unselected)}
    checks = {"coverage_complete": all(held[a][k]["coverage_complete"] for a in ARMS for k in ("base", "stress")),
              "every_arm_selected": True}
    for arm in ARMS:
        base = held[arm]["base"]
        checks[f"{arm}_independent_positions_sufficient"] = base["independent_positions"] >= MIN_TEST_POSITIONS
        if arm_direction(arm) == "both":
            checks[f"{arm}_positions_per_direction_sufficient"] = (
                base["long_positions"] >= MIN_SIDE_POSITIONS and base["short_positions"] >= MIN_SIDE_POSITIONS)
    for arm in CANDIDATE_ARMS:
        checks[f"{arm}_neighbours_evaluable"] = stab[arm]["evaluable"] > 0
    performance = {"no_liquidated_leg": all(held[a][k]["liquidated_legs"] == 0 for a in ARMS
                                            for k in ("base", "stress"))}
    for arm in CANDIDATE_ARMS:
        baseline = SEED["arms"][arm]["baseline"]
        c, b = held[arm]["base"], held[baseline]["base"]
        performance[f"{arm}_net_positive_base"] = c["net_pnl_usd"] > 0
        performance[f"{arm}_net_positive_stress"] = held[arm]["stress"]["net_pnl_usd"] > 0
        performance[f"{arm}_beats_{baseline}_sharpe"] = c["mean_sharpe"] > b["mean_sharpe"]
        performance[f"{arm}_beats_{baseline}_ddadj"] = c["mean_ddadj"] > b["mean_ddadj"]
        performance[f"{arm}_neighbour_stability"] = stab[arm]["met"]
    if not all(checks.values()):
        failed = sorted(k for k, ok in checks.items() if not ok)
        return {"outcome": "inconclusive", "checks": checks, "performance_checks_descriptive": performance,
                "reason": f"{prov} held-out coverage, sample or stability-evaluability gate not met: "
                          + ", ".join(failed)}
    checks.update(performance)
    if all(checks.values()):
        return {"outcome": "pass", "checks": checks, "reason": f"every frozen {prov} held-out check passed"}
    failed = sorted(k for k, ok in checks.items() if not ok)
    return {"outcome": "fail", "checks": checks, "reason": f"failed frozen {prov} checks: " + ", ".join(failed)}


def overall_verdict(venue: dict, proxy: dict) -> dict:
    if venue["outcome"] in ("fail", "inconclusive"):
        outcome = venue["outcome"]
        reason = f"venue (primary) verdict {venue['outcome']}: {venue['reason']}"
    elif proxy["outcome"] == "pass":
        outcome, reason = "pass", "venue and proxy verdicts both pass"
    else:
        outcome = "inconclusive"
        reason = f"venue passes but the proxy verdict is {proxy['outcome']}: {proxy['reason']}"
    edge = {"pass": "unvalidated", "fail": "study_fail", "inconclusive": "study_inconclusive"}[outcome]
    return {"outcome": outcome, "reason": reason, "edge_source": edge, "live_status": LIVE_STATUS}


def _fmt_full(value) -> str:
    return repr(float(value)) if isinstance(value, (float, np.floating)) else str(value)


def _row_hash(frame, lo: int, hi: int) -> str:
    lines = []
    for i in range(lo, hi + 1):
        row = frame.iloc[i]
        lines.append(",".join([str(int(row["timestamp"]))] + [_fmt_full(row[c]) for c in ("open", "high", "low",
                                                                                          "close")]))
    return hashlib.sha256(("\n".join(lines) + "\n").encode("utf-8")).hexdigest()


def write_evidence(reg, manifest: dict, arm: str, params: dict) -> tuple:
    import offline_manifest as om
    import pandas as pd
    os.makedirs(EVIDENCE_DIR, exist_ok=True)
    files, audit = [], []
    need = int(params["recursion_length"]) + int(params["range_period"]) + 1
    for ds in manifest["datasets"]:
        for window_name in (SELECTION_WINDOW, HELD_OUT_WINDOW):
            frame, win, _ = om.window_frame(manifest, ds, window_name)
            result = reg.apply_strategy(CANDIDATE, frame, params)
            path = os.path.join(EVIDENCE_DIR, f"{arm}_{ds['coin']}_{window_name}.csv.gz")
            header = list(EVIDENCE_INPUT_COLUMNS) + list(EVIDENCE_VALUE_COLUMNS) + ["signal", "smi_reason", "scored"]
            ts = frame["timestamp"].to_numpy(dtype=np.int64)
            scored = (ts >= win["start_ms"]) & (ts < win["end_ms"])
            lines = [",".join(header)]
            for i in range(len(result)):
                row = result.iloc[i]
                cells = [str(int(row["timestamp"]))]
                cells += [_fmt_full(row[c]) for c in EVIDENCE_INPUT_COLUMNS[1:]]
                cells += [_fmt_full(row[c]) for c in EVIDENCE_VALUE_COLUMNS]
                cells += [str(int(row["signal"])), str(row["smi_reason"]), str(int(scored[i]))]
                lines.append(",".join(cells))
            payload = ("\n".join(lines) + "\n").encode("utf-8")
            with open(path, "wb") as raw:
                with gzip.GzipFile(filename="", mode="wb", fileobj=raw, mtime=0) as fh:
                    fh.write(payload)
            sig = result["signal"].to_numpy()
            for i in np.flatnonzero((sig != 0) & scored):
                i = int(i)
                audit.append({"arm": arm, "dataset": ds["key"], "window": window_name,
                              "bar": str(pd.to_datetime(int(ts[i]), unit="ms")), "side": int(sig[i]),
                              "dependency_first_bar": str(pd.to_datetime(int(ts[i - need + 1]), unit="ms")),
                              "dependency_rows": need, "dependency_sha256": _row_hash(frame, i - need + 1, i),
                              "smi_value": float(result["smi_value"].iloc[i]),
                              "smi_prev_value": float(result["smi_prev_value"].iloc[i]),
                              "smi_diff": float(result["smi_diff"].iloc[i]),
                              "smi_prev_diff": float(result["smi_prev_diff"].iloc[i])})
            reasons = pd.Series(result["smi_reason"].to_numpy()[scored]).value_counts().to_dict()
            files.append({"arm": arm, "dataset": ds["key"], "window": window_name, "params": params,
                          "path": os.path.relpath(path, _REPO), "sha256": sha256_file(path), "rows": len(result),
                          "scored_rows": int(scored.sum()), "entry_signals_scored": int(((sig != 0) & scored).sum()),
                          "scored_reason_counts": {str(k): int(v) for k, v in sorted(reasons.items())}})
    return files, audit


def continuous_series(high, low, close, params: dict) -> tuple:
    k = int(params["range_period"])
    a = 2.0 / (int(params["smoothing_period"]) + 1)
    b = 2.0 / (int(params["signal_period"]) + 1)
    n = len(close)
    value = np.full(n, np.nan)
    line = np.full(n, np.nan)
    n1 = n2 = d1 = d2 = sig = None
    for i in range(k - 1, n):
        hh = float(np.max(high[i - k + 1:i + 1]))
        ll = float(np.min(low[i - k + 1:i + 1]))
        rel = float(close[i]) - (hh + ll) / 2.0
        rng = hh - ll
        if n1 is None:
            n1 = n2 = rel
            d1 = d2 = rng
        else:
            n1 = a * rel + (1.0 - a) * n1
            n2 = a * n1 + (1.0 - a) * n2
            d1 = a * rng + (1.0 - a) * d1
            d2 = a * d1 + (1.0 - a) * d2
        if not d2 > 0:
            continue
        osc = 200.0 * n2 / d2
        sig = osc if sig is None else b * osc + (1.0 - b) * sig
        value[i] = osc
        line[i] = sig
    return value, line


def continuous_reference(reg, manifest: dict, params: dict) -> dict:
    import offline_manifest as om
    lo_t, hi_t = float(params["oversold"]), float(params["overbought"])
    out = {"params": params, "datasets": []}
    keys = ("scored_rows", "finite_valid_rows", "finite_entries", "continuous_entries", "decision_mismatches",
            "finite_hold_continuous_entry", "both_valid_different_decision")
    totals = {k: 0 for k in keys}
    for ds in manifest["datasets"]:
        candles = om.load_candles(ds)
        h, lo, c = (candles[k].to_numpy(dtype=float) for k in ("high", "low", "close"))
        val, line = continuous_series(h, lo, c, params)
        diff = val - line
        pv, pd_ = np.r_[np.nan, val[:-1]], np.r_[np.nan, diff[:-1]]
        with np.errstate(invalid="ignore"):
            cont = np.where((pv <= lo_t) & (val > lo_t) & (pd_ <= 0) & (diff > 0), 1,
                            np.where((pv >= hi_t) & (val < hi_t) & (pd_ >= 0) & (diff < 0), -1, 0))
        ts_all = candles["timestamp"].to_numpy(dtype=np.int64)
        for window_name in (SELECTION_WINDOW, HELD_OUT_WINDOW):
            frame, win, _ = om.window_frame(manifest, ds, window_name)
            finite = reg.apply_strategy(CANDIDATE, frame, params)
            ts = frame["timestamp"].to_numpy(dtype=np.int64)
            pos = np.searchsorted(ts_all, ts)
            scored = (ts >= win["start_ms"]) & (ts < win["end_ms"])
            fsig = finite["signal"].to_numpy()
            fvalid = finite["smi_valid"].to_numpy() == 1.0
            csig = cont[pos]
            mism = scored & (fsig != csig)
            fv = finite["smi_value"].to_numpy()
            both = scored & fvalid & np.isfinite(val[pos])
            vdiff = np.abs(fv[both] - val[pos][both])
            rec = {"dataset": ds["key"], "window": window_name, "scored_rows": int(scored.sum()),
                   "finite_valid_rows": int((scored & fvalid).sum()),
                   "finite_entries": int((scored & (fsig != 0)).sum()),
                   "continuous_entries": int((scored & (csig != 0)).sum()),
                   "decision_mismatches": int(mism.sum()),
                   "finite_hold_continuous_entry": int((mism & ~fvalid).sum()),
                   "both_valid_different_decision": int((mism & fvalid).sum()),
                   "value_abs_diff_max": float(vdiff.max()) if len(vdiff) else None,
                   "value_abs_diff_p95": float(np.percentile(vdiff, 95)) if len(vdiff) else None}
            out["datasets"].append(rec)
            for k in keys:
                totals[k] += rec[k]
    out["totals"] = totals
    return out


def _manifest_record(manifest: dict) -> dict:
    return {
        "path": os.path.relpath(manifest["path"], _REPO), "sha256": sha256_file(manifest["path"]),
        "study": manifest["study"], "provenance": manifest["provenance"], "interval": manifest["interval"],
        "warmup_bars": manifest["warmup_bars"],
        "windows": {k: {"start": v["start"], "end": v["end"], "role": v["role"]} for k, v in manifest["windows"].items()},
        "costs": manifest["costs"], "acquisition": manifest["acquisition"],
        "datasets": [{"key": d["key"], "size_decimals": d["size_decimals"], "half_spread_bps": d["half_spread_bps"],
                      "candles_path": d["candles"]["path"], "candles_sha256": d["candles"]["sha256"],
                      "funding_path": d["funding"]["path"] if d["funding"] else None,
                      "funding_sha256": d["funding"]["sha256"] if d["funding"] else None}
                     for d in manifest["datasets"]],
    }


def run(workers: int) -> dict:
    inventory = proxy_inventory()
    tmp = tempfile.mkdtemp(prefix="smi_1663_cache_")
    db_path = os.path.join(tmp, "proxy_cache.db")
    os.environ[CACHE_ENV] = db_path
    import offline_manifest as om
    build_proxy_cache(inventory, db_path)
    forbid_network()
    import storage
    if os.path.abspath(storage.DB_PATH) != os.path.abspath(db_path):
        raise SystemExit(f"cache {storage.DB_PATH} is not the private study cache")
    from eval_windows import INCUMBENTS
    from registry_loader import load_registry
    reg = load_registry("futures")
    manifest = om.load_manifest(MANIFEST)
    gates = {"venue": venue_preflight(manifest), "proxy": proxy_preflight(inventory)}
    coverage = {f"{r['dataset']}|{r['window']}": r for r in gates["proxy"]["windows"]}
    window_bars = {}
    for r in gates["venue"]["windows"]:
        window_bars[("venue", f"{r['dataset']}|{r['window']}")] = (r.get("candle_coverage") or {}).get("present_bars")
    for r in gates["proxy"]["windows"]:
        window_bars[("proxy", f"{r['dataset']}|{r['window']}")] = r["present_bars"]
    result = {
        "study": "stochastic_momentum_index_1663", "issue": SEED["issue"], "seed_sha256": sha256_file(SEED_PATH),
        "protocol_sha256": protocol_sha256(), "seed": SEED, "manifest": _manifest_record(manifest),
        "proxy_inputs": {k: v for k, v in inventory.items()},
        "source_sha256": {p: sha256_file(os.path.join(_REPO, p)) for p in SOURCE_FILES},
        "coverage_gates": gates,
    }
    runner = Runner(db_path, workers)
    try:
        def summ(prov, res, window, detail=True):
            bars = {k: v for (p, k), v in window_bars.items() if p == prov}
            return summarize(res, prov, window, coverage, bars, keep_detail=detail)

        arms = {}
        selections = {}
        if gates["venue"]["complete"]:
            for arm in ARMS:
                name = arm_strategy(arm)
                defaults = dict(reg.STRATEGY_REGISTRY[name]["default_params"])
                grid = grid_rows(name, defaults)
                print(f"[train] {arm}: {len(grid)} combinations", flush=True)
                res = runner.run([leg_task("venue", arm, p, SELECTION_WINDOW, VENUE_BASE) for _, p in grid])
                rows = [{"label": label, "params": p, "summary": summ("venue", r, SELECTION_WINDOW, False)}
                        for (label, p), r in zip(grid, res)]
                choice = select(rows)
                selections[arm] = choice["params"]
                arms[arm] = {"strategy": name, "direction": arm_direction(arm), "defaults": defaults,
                             "grid_size": len(grid), "train_sweep": rows, "selection": choice}
        else:
            selections = {a: None for a in ARMS}
        result["arms"] = arms
        result["selections"] = selections

        held = {p: {} for p in PROVENANCES}
        defaults_held = {}
        stab = {p: {} for p in PROVENANCES}
        variants = []
        incumbents = {}
        if all(selections.get(a) is not None for a in ARMS):
            costs = {"venue": (VENUE_BASE, VENUE_STRESS), "proxy": (PROXY_BASE, PROXY_STRESS)}
            for prov in PROVENANCES:
                base_c, stress_c = costs[prov]
                for arm in ARMS:
                    p = selections[arm]
                    base, stress, train = runner.run([leg_task(prov, arm, p, HELD_OUT_WINDOW, base_c),
                                                      leg_task(prov, arm, p, HELD_OUT_WINDOW, stress_c),
                                                      leg_task(prov, arm, p, SELECTION_WINDOW, base_c)])
                    held[prov][arm] = {"params": p, "base": summ(prov, base, HELD_OUT_WINDOW),
                                       "stress": summ(prov, stress, HELD_OUT_WINDOW),
                                       "train": summ(prov, train, SELECTION_WINDOW)}
                for arm in CANDIDATE_ARMS:
                    neigh = neighbour_params(selections[arm])
                    res = runner.run([leg_task(prov, arm, q, HELD_OUT_WINDOW, base_c) for q in neigh])
                    rows = []
                    for q, r in zip(neigh, res):
                        s = summ(prov, r, HELD_OUT_WINDOW, False)
                        rows.append({"params": q, "net_pnl_usd": s.get("net_pnl_usd"),
                                     "mean_sharpe": s.get("mean_sharpe"), "mean_ddadj": s.get("mean_ddadj"),
                                     "positions": s["positions"], "independent_positions": s["independent_positions"],
                                     "coverage_complete": s["coverage_complete"], "datasets": s["datasets"]})
                    stab[prov][arm] = stability(rows)
            for arm in ARMS:
                name = arm_strategy(arm)
                d = dict(reg.STRATEGY_REGISTRY[name]["default_params"])
                base, stress = runner.run([leg_task("venue", arm, d, HELD_OUT_WINDOW, VENUE_BASE),
                                           leg_task("venue", arm, d, HELD_OUT_WINDOW, VENUE_STRESS)])
                defaults_held[arm] = {"params": d, "base": summ("venue", base, HELD_OUT_WINDOW),
                                      "stress": summ("venue", stress, HELD_OUT_WINDOW)}
            for arm in CANDIDATE_ARMS:
                rows = arms[arm]["train_sweep"]
                print(f"[held-out variants] {arm}: {len(rows)} combinations", flush=True)
                res = runner.run([leg_task("venue", arm, r["params"], HELD_OUT_WINDOW, VENUE_BASE) for r in rows])
                for r, t in zip(rows, res):
                    s = summ("venue", t, HELD_OUT_WINDOW, False)
                    variants.append({"arm": arm, "label": r["label"], "params": r["params"],
                                     "train_eligible": eligible(r["summary"]),
                                     "train_mean_sharpe": r["summary"].get("mean_sharpe"),
                                     "train_independent_positions": r["summary"]["independent_positions"],
                                     "test_mean_sharpe": s.get("mean_sharpe"), "test_mean_ddadj": s.get("mean_ddadj"),
                                     "test_net_pnl_usd": s.get("net_pnl_usd"), "test_positions": s["positions"],
                                     "test_independent_positions": s["independent_positions"],
                                     "test_coverage_complete": s["coverage_complete"]})
            inc_tasks = [("incumbents", "venue", SELECTION_WINDOW, VENUE_BASE),
                         ("incumbents", "venue", HELD_OUT_WINDOW, VENUE_BASE),
                         ("incumbents", "venue", HELD_OUT_WINDOW, VENUE_STRESS),
                         ("incumbents", "proxy", HELD_OUT_WINDOW, PROXY_BASE)]
            for t, r in zip(inc_tasks, runner.run(inc_tasks)):
                incumbents[f"{t[1]} {t[2]} x{t[3]:g}"] = r
    finally:
        runner.close()

    verdicts = {}
    for prov in PROVENANCES:
        if not all(selections.get(a) is not None for a in ARMS):
            verdicts[prov] = provenance_verdict(prov, gates["venue"] if prov == "venue" else gates["proxy"],
                                                {a: selections.get(a) for a in ARMS}, {}, {})
            continue
        verdicts[prov] = provenance_verdict(prov, gates[prov], selections, held[prov], stab[prov])
    stability_summary = {}
    for arm in CANDIDATE_ARMS:
        rows = [v for v in variants if v["arm"] == arm]
        scored = [v for v in rows if v["train_mean_sharpe"] is not None and v["test_mean_sharpe"] is not None]
        stability_summary[arm] = {
            "spearman_train_vs_test_sharpe": spearman([v["train_mean_sharpe"] for v in scored],
                                                      [v["test_mean_sharpe"] for v in scored]),
            "test_net_positive_variants": sum(1 for v in rows if (v["test_net_pnl_usd"] or 0) > 0),
            "train_eligible_variants": sum(1 for v in rows if v["train_eligible"]),
            "variants": len(rows),
        }
    evidence, audit = [], []
    continuous = None
    if all(selections.get(a) is not None for a in CANDIDATE_ARMS):
        for arm in CANDIDATE_ARMS:
            files, entries = write_evidence(reg, manifest, arm, selections[arm])
            evidence += files
            audit += entries
        continuous = continuous_reference(reg, manifest, selections["smi_both"])
    result.update({
        "held_out": held, "held_out_defaults_venue": defaults_held, "neighbour_stability": stab,
        "candidate_variants_held_out_venue": variants, "parameter_stability_venue": stability_summary,
        "incumbent_reference": incumbents, "incumbent_members": list(INCUMBENTS),
        "evidence": evidence, "entry_audit": audit, "continuous_reference": continuous,
        "verdicts": verdicts, "verdict": overall_verdict(verdicts["venue"], verdicts["proxy"]),
    })
    return result


def _fmt(v, nd=2):
    if v is None:
        return "-"
    if isinstance(v, bool):
        return "yes" if v else "no"
    if isinstance(v, float):
        if not math.isfinite(v):
            return str(v)
        return f"{v:.{nd}f}"
    return str(v)


def _summary_row(arm: str, label: str, s: dict) -> str:
    if not s or not s.get("datasets") or "net_pnl_usd" not in s:
        return f"| `{arm}` | {label} | no scored dataset |" + " |" * 12
    return (f"| `{arm}` | {label} | {_fmt(s['net_pnl_usd'])} | {_fmt(s['mean_return_pct'])} | "
            f"{_fmt(s['mean_sharpe'], 3)} | {_fmt(s['mean_ddadj'], 3)} | {_fmt(s['worst_max_dd_pct'])} | "
            f"{_fmt(s['fees_usd'])} | {_fmt(s['funding_pnl_usd'])} | {_fmt(s['mean_turnover'])} | "
            f"{s['positions']} ({s['long_positions']}/{s['short_positions']}) | "
            f"{_fmt(s['long_net_pnl_usd'])} / {_fmt(s['short_net_pnl_usd'])} | {s['independent_positions']} | "
            f"{_fmt(s['top_dataset_abs_pnl_share'])} | {_fmt(s['top5_position_abs_pnl_share'])} |")


SUMMARY_HEADER = ("| Arm | Run | Net $ | Mean ret % | Mean Sharpe | Mean DDadj | Worst DD % | Fees $ | Funding $ | "
                  "Turnover | Positions (L/S) | Long / short net $ | Independent | Top dataset share | Top-5 share |")
SUMMARY_SEP = "|---|---|---|---|---|---|---|---|---|---|---|---|---|---|---|"


def render(result: dict) -> str:
    v = result["verdict"]
    m = result["manifest"]
    seed = result["seed"]
    pi = result["proxy_inputs"]
    lines = [
        "# Stochastic Momentum Index pullback entries: held-out result (#1663)",
        "",
        f"Overall verdict: **{v['outcome'].upper()}**. {v['reason']}.",
        "",
        f"Edge metadata: `edge_status=no_edge`, `edge_source={v['edge_source']}`. Availability: {v['live_status']}.",
        "",
        "Generated by `run_study.py` from `results.json`; do not edit by hand. The frozen specification is in "
        f"`README.md` and `candidate_seed.json` (file sha256 `{result['seed_sha256']}`, protocol sha256 "
        f"`{result['protocol_sha256']}`). Both windows were used by earlier candidate studies on the same inputs, "
        "so this is retrospective candidate evidence.",
        "",
        "Funding dependency #1731 is complete (merged pull request #1745). This study reuses the manifest funding "
        "attachment and the cached M1 `--funding charge` path; it adds no funding option and no cost owner.",
        "",
        "## Verdicts by provenance",
        "",
    ]
    for prov in PROVENANCES:
        pv = result["verdicts"][prov]
        lines.append(f"- {prov}: **{pv['outcome']}**. {pv['reason']}.")
    lines += [
        "- Overall rule: " + seed["verdict"]["overall"] + ".",
        "",
        "## Inputs",
        "",
        f"- Venue (primary): manifest `{m['path']}` sha256 `{m['sha256']}`; {m['provenance']['label']}; "
        f"{m['acquisition'].get('reused_from', '-')}.",
    ]
    c = m["costs"]
    lines.append(
        f"- Venue costs: the manifest execution specification for explicit-close arms (taker "
        f"{c['taker_fee_pct']*100:.3f}%, maker {c['maker_fee_pct']*100:.3f}%, slippage {c['slippage_bps']} bps per "
        f"side plus the per-coin half spread, szDecimals lot flooring, minimum order ${c['min_notional_usd']:.0f} "
        f"with a {c['min_notional_margin']*100:.0f}% margin) and hourly manifest funding; stress doubles half spread "
        "and slippage.")
    for d in m["datasets"]:
        lines.append(f"  - `{d['key']}`: szDecimals {d['size_decimals']}, half spread {d['half_spread_bps']} bps, "
                     f"candles `{d['candles_path']}` `{d['candles_sha256'][:16]}`, funding `{d['funding_path']}` "
                     f"`{(d['funding_sha256'] or '')[:16]}`")
    lines.append(
        f"- Proxy (secondary): `{pi['inventory_path']}` sha256 `{pi['inventory_sha256'][:16]}`; {pi['candle_source']}; "
        f"{pi['funding_source']}. The files are referenced in place, verified against the inventory and loaded into "
        "a private scratch cache with network access disabled.")
    for f in pi["files"]:
        lines.append(f"  - `{f['path']}` `{f['sha256'][:16]}`")
    lines.append(
        "- Proxy costs: Hyperliquid fees and the default slippage, which stands in for the spread on this path; "
        "the cached path has no venue lot or minimum rule; funding `--funding charge`; stress doubles slippage.")
    lines.append(f"- Interval 4h, warm-up {seed['warmup_bars']} bars (not scored).")
    for name, (start, end, role) in seed["windows"].items():
        lines.append(f"- Window `{name}` [{start}, {end}): {role}")
    lines.append(
        "- M1 incumbents run under the frozen long-only incumbent contract (same close and stop owner, capital and "
        "costs) and are descriptive only.")
    lines.append(
        "- Unmodeled: queue position, partial fills, depth beyond the frozen half spread, intra-hour funding "
        "changes, liquidation-engine fees and venue outages.")
    for prov in PROVENANCES:
        gate = result["coverage_gates"][prov]
        lines += ["", f"## Coverage gate ({prov})", "", f"Complete: {_fmt(gate['complete'])}.", ""]
        if prov == "venue":
            lines += ["| Dataset | Window | Candle bars present/expected | Warm-up missing | Funding hours present/expected "
                      "| Accrual slots missing | Complete |", "|---|---|---|---|---|---|---|"]
            for r in gate["windows"]:
                cc, fc, ac = r.get("candle_coverage") or {}, r.get("funding_coverage") or {}, r.get("accrual_span") or {}
                lines.append(f"| {r['dataset']} | {r['window']} | {cc.get('present_bars', '-')}/"
                             f"{cc.get('expected_bars', '-')} | {cc.get('warmup_missing_bars', '-')} | "
                             f"{fc.get('present_hours', '-')}/{fc.get('expected_hours', '-')} | "
                             f"{ac.get('missing_slots', '-')} | {_fmt(r['complete'])} |")
        else:
            lines += ["| Dataset | Window | Bars present/expected | Missing bars | Warm-up missing | Venue-absent funding "
                      "hours in span | Complete |", "|---|---|---|---|---|---|---|"]
            for r in gate["windows"]:
                miss = ", ".join(r["missing_bar_times"]) or "none"
                gaps = ", ".join(f"{g[0]}" for g in r["funding_venue_gap_hours_in_span"]) or "none"
                lines.append(f"| {r['dataset']} | {r['window']} | {r['present_bars']}/{r['expected_bars']} | {miss} | "
                             f"{r['warmup_missing_bars']} | {gaps} | {_fmt(r['complete'])} |")
            lines.append("")
            lines.append("Venue-absent funding hours are hours the venue itself never printed; the existing funding "
                         "owner charges them zero by its recorded-gap contract. They are not missing data.")
        for r in gate["refused"]:
            lines.append(f"- REFUSED {r['dataset']} {r['window']}: {'; '.join(r['problems'])}")
    sel = seed["selection"]
    lines += [
        "", "## Protocol", "",
        f"- Close owner `tiered_tp_pct` with one tier `{json.dumps(seed['close_strategies'][0]['params'], sort_keys=True)}`, "
        f"comparison mode `{seed['comparison_mode']}`, stop owner fixed entry ATR x{seed['stop_loss_atr_mult']} "
        f"({seed['atr_method']} ATR), capital ${seed['capital']:.0f}, leverage {seed['leverage']:g}, "
        f"`{seed['intrabar_resolution']}` intrabar resolution; signals on bar N fill at the bar N+1 open.",
        "- Arms: " + ", ".join(f"`{n}` (`{seed['arms'][n]['strategy']}`, {seed['arms'][n]['direction']})"
                               for n in ARMS) + ". Matched comparisons: `smi_long` vs `stoch_rsi` (long only) and "
        "`smi_both` vs `mean_reversion_pro` (both directions).",
        f"- Selection on venue `{seed['selection_window']}` only: {sel['eligible']}; order {'; '.join(sel['order'])}. "
        "The proxy evidence uses the venue selections; nothing is reselected from held-out or proxy results.",
        f"- Held-out gates: at least {MIN_TEST_POSITIONS} independent position clusters per arm and at least "
        f"{MIN_SIDE_POSITIONS} positions in each direction for the bidirectional arms. "
        + seed["independent_positions"] + ".",
        f"- Stability: {seed['stability']['neighbours']}; {seed['stability']['rule']}.",
        f"- Pass rule: {seed['verdict']['pass']}.",
        "- `stoch_rsi` keeps its registered long-only entries: its -1 is a long exit and is never treated as a short "
        "entry, so its comparison uses the matched long-only candidate arm.",
    ]
    if not result.get("arms"):
        lines += ["", "No selection or score was computed because the venue coverage gate refused a window.", ""]
        return "\n".join(lines)
    lines += ["", "## Selections (venue train)", "",
              "| Arm | Direction | Eligible combos | Selected params | Train mean Sharpe | Train independent positions |",
              "|---|---|---|---|---|---|"]
    for arm in ARMS:
        a = result["arms"][arm]
        s = a["selection"]
        row = next((r for r in a["train_sweep"] if r["label"] == s.get("selected")), None)
        lines.append(f"| `{arm}` | {a['direction']} | {s['eligible_combos']}/{a['grid_size']} | "
                     f"{('`' + json.dumps(s['params'], sort_keys=True) + '`') if s['params'] else 'none'} | "
                     f"{_fmt(row['summary']['mean_sharpe'], 3) if row else '-'} | "
                     f"{row['summary']['independent_positions'] if row else '-'} |")
    for prov in PROVENANCES:
        held = result["held_out"].get(prov) or {}
        if not held:
            continue
        lines += ["", f"## Held-out comparison ({prov})", "",
                  "Selected rows are verdict inputs; train rows are descriptive.", "", SUMMARY_HEADER, SUMMARY_SEP]
        base_lbl, stress_lbl = ("x1", "x2")
        for arm in ARMS:
            t = held[arm]
            lines.append(_summary_row(arm, f"held-out base {base_lbl}", t["base"]))
            lines.append(_summary_row(arm, f"held-out stress {stress_lbl}", t["stress"]))
            lines.append(_summary_row(arm, f"train base {base_lbl}", t["train"]))
        for arm in CANDIDATE_ARMS:
            lines += ["", f"### `{arm}` per dataset ({prov}, held-out, base cost)", "",
                      "| Dataset | Net $ | Ret % | Sharpe | Max DD % | Fees $ | Funding $ | Positions (L/S) | "
                      "Time in market | Rejected entries | Skipped partial closes | Cost model |",
                      "|---|---|---|---|---|---|---|---|---|---|---|---|"]
            for ds, s in sorted((held[arm]["base"].get("per_dataset") or {}).items()):
                lines.append(f"| {ds} | {_fmt(s['net_pnl_usd'])} | {_fmt(s['return_pct'])} | {_fmt(s['sharpe'], 3)} | "
                             f"{_fmt(s['max_dd_pct'])} | {_fmt(s['fees_usd'])} | {_fmt(s['funding_pnl_usd'])} | "
                             f"{s['positions']} ({s['long_positions']}/{s['short_positions']}) | "
                             f"{_fmt(s['time_in_market'])} | {s['rejected_entries']} | {s['skipped_partial_closes']} | "
                             f"{s['cost_model']} |")
        lines += ["", f"### Neighbour stability ({prov}, held-out, base cost)", ""]
        for arm in CANDIDATE_ARMS:
            st = result["neighbour_stability"][prov][arm]
            lines.append(f"- `{arm}`: {st['net_positive']}/{st['evaluable']} evaluable one-step neighbours net positive "
                         f"(share {_fmt(st['net_positive_share'], 3)}; rule met {_fmt(st['met'])}).")
        lines += ["", "| Arm | Neighbour params | Net $ | Mean Sharpe | Mean DDadj | Positions | Independent | Complete |",
                  "|---|---|---|---|---|---|---|---|"]
        for arm in CANDIDATE_ARMS:
            for r in result["neighbour_stability"][prov][arm]["rows"]:
                lines.append(f"| `{arm}` | `{json.dumps(r['params'], sort_keys=True)}` | {_fmt(r['net_pnl_usd'])} | "
                             f"{_fmt(r['mean_sharpe'], 3)} | {_fmt(r['mean_ddadj'], 3)} | {r['positions']} | "
                             f"{r['independent_positions']} | {_fmt(r['coverage_complete'])} |")
    if result.get("held_out_defaults_venue"):
        lines += ["", "## Registry defaults (venue held-out, descriptive)", "", SUMMARY_HEADER, SUMMARY_SEP]
        for arm in ARMS:
            t = result["held_out_defaults_venue"][arm]
            lines.append(_summary_row(arm, "defaults base x1", t["base"]))
            lines.append(_summary_row(arm, "defaults stress x2", t["stress"]))
    lines += ["", "## Verdict checks", ""]
    for prov in PROVENANCES:
        pv = result["verdicts"][prov]
        lines.append(f"- {prov}:")
        for k, ok in sorted(pv["checks"].items()):
            lines.append(f"  - {k}: {_fmt(ok)}")
        if pv.get("performance_checks_descriptive"):
            lines.append("  - descriptive performance checks (a gate stopped the verdict first): "
                         + ", ".join(f"{k} {_fmt(ok)}" for k, ok in sorted(pv["performance_checks_descriptive"].items())))
    if result.get("incumbent_reference"):
        lines += ["", "## M1 incumbent aggregate (descriptive, not a verdict input)", "",
                  "Members: " + ", ".join(f"`{n}`" for n in result["incumbent_members"]) + ". Registry defaults, run "
                  "long only under the frozen close, stop, capital and cost contract; the aggregate is the median per "
                  "dataset. The bidirectional `smi_both` comparison stays separate.", "",
                  "| Window and cost | Dataset | Median Sharpe | Median DDadj | Legs | Funding incomplete |",
                  "|---|---|---|---|---|---|"]
        for key, rec in sorted(result["incumbent_reference"].items()):
            for ds, bar in sorted((rec.get("bars") or {}).items()):
                if bar is None:
                    lines.append(f"| {key} | {ds} | - | - | 0 | - |")
                    continue
                lines.append(f"| {key} | {ds} | {_fmt(bar['sharpe'], 3)} | {_fmt(bar['ddadj'], 3)} | {bar['n']} | "
                             f"{_fmt(bar['funding_incomplete'])} |")
    cr = result.get("continuous_reference")
    if cr:
        tot = cr["totals"]
        lines += ["", "## Finite replay versus a continuously seeded diagnostic", "",
                  f"Parameters `{json.dumps(cr['params'], sort_keys=True)}` (the selected `smi_both` parameters), venue "
                  "frames. The diagnostic seeds every recursion once at the first complete range window of the whole "
                  "manifest frame and carries it forward. It is a separately named research diagnostic; the finite "
                  "replay stays the candidate definition and its acceptance.", "",
                  f"- Scored rows {tot['scored_rows']}; finite valid rows {tot['finite_valid_rows']}; entries finite "
                  f"{tot['finite_entries']}, continuous {tot['continuous_entries']}; decision mismatches "
                  f"{tot['decision_mismatches']} (finite hold where continuous enters "
                  f"{tot['finite_hold_continuous_entry']}, both valid and different {tot['both_valid_different_decision']}).",
                  "", "| Dataset | Window | Scored | Finite valid | Finite entries | Continuous entries | Mismatches | "
                  "Max abs value diff | p95 abs value diff |", "|---|---|---|---|---|---|---|---|---|"]
        for r in cr["datasets"]:
            lines.append(f"| {r['dataset']} | {r['window']} | {r['scored_rows']} | {r['finite_valid_rows']} | "
                         f"{r['finite_entries']} | {r['continuous_entries']} | {r['decision_mismatches']} | "
                         f"{_fmt(r['value_abs_diff_max'], 6)} | {_fmt(r['value_abs_diff_p95'], 6)} |")
    st = result.get("parameter_stability_venue") or {}
    if st:
        lines += ["", "## Parameter stability and unsuccessful variants (venue)", ""]
        for arm in CANDIDATE_ARMS:
            s = st[arm]
            lines.append(f"- `{arm}`: training-eligible variants {s['train_eligible_variants']}/{s['variants']}; "
                         f"Spearman train vs held-out mean Sharpe {_fmt(s['spearman_train_vs_test_sharpe'], 3)}; "
                         f"held-out net-positive variants {s['test_net_positive_variants']}/{s['variants']}.")
        lines += ["", "Every grid variant of both candidate arms, held-out at base cost (descriptive; no reselection).", "",
                  "| Arm | Variant | Train eligible | Train Sharpe | Train independent | Held-out Sharpe | Held-out DDadj | "
                  "Held-out net $ | Held-out positions | Held-out independent |", "|---|---|---|---|---|---|---|---|---|---|"]
        for row in result["candidate_variants_held_out_venue"]:
            lines.append(f"| `{row['arm']}` | {row['label']} | {_fmt(row['train_eligible'])} | "
                         f"{_fmt(row['train_mean_sharpe'], 3)} | {row['train_independent_positions']} | "
                         f"{_fmt(row['test_mean_sharpe'], 3)} | {_fmt(row['test_mean_ddadj'], 3)} | "
                         f"{_fmt(row['test_net_pnl_usd'])} | {row['test_positions']} | {row['test_independent_positions']} |")
    cv = ((result["held_out"].get("venue") or {}).get("smi_both") or {}).get("base", {}).get("close_validation") or {}
    if cv:
        lines += ["", "## Close validation", "",
                  f"- Modes `{', '.join(cv.get('modes') or [str(cv.get('mode'))])}`, eligibility "
                  f"`{cv.get('close_eligibility')}`, parity status `{cv.get('parity_status')}`, incomplete parity "
                  f"{_fmt(cv.get('incomplete_parity'))}.",
                  "- `unverified` is the strict, eligible capability status (no approximation and no refusal); it is not "
                  "a ledger-verified claim that these exits equal live exits."]
    if os.path.exists(PARITY_PATH):
        with open(PARITY_PATH) as fh:
            parity = json.load(fh)
        lines += ["", "## Decision parity", "",
                  f"`{os.path.relpath(PARITY_PATH, _REPO)}` sha256 `{sha256_file(PARITY_PATH)[:16]}` from `run_parity.py`, "
                  f"clean {_fmt(parity['clean'])}.", ""]
        for key, block in sorted(parity["sections"].items()):
            lines.append(f"- {key}: {block['summary']}")
        lines.append("- Admission and timed closure are proved by the subprocess scenarios in "
                     "`shared_scripts/test_stochastic_momentum_index_checks.py`; a parity run alone does not prove "
                     "forming-row isolation.")
    if result.get("evidence"):
        lines += ["", "## Full-precision evidence and entry audit", "",
                  "Every input row and every evaluator value of the selected candidate parameters, written with `repr` "
                  f"floats. `results.json` `entry_audit` lists {len(result['entry_audit'])} scored entries with the sha256 "
                  "of their dependency rows.", "",
                  "| Arm | Dataset | Window | File | sha256 | Rows | Scored | Scored entries | Scored reasons |",
                  "|---|---|---|---|---|---|---|---|---|"]
        for e in result["evidence"]:
            reasons = ", ".join(f"{k} {n}" for k, n in e["scored_reason_counts"].items())
            lines.append(f"| `{e['arm']}` | {e['dataset']} | {e['window']} | `{e['path']}` | `{e['sha256'][:16]}` | "
                         f"{e['rows']} | {e['scored_rows']} | {e['entry_signals_scored']} | {reasons} |")
    lines += ["", "## Reproduce", "",
              "- `uv run --no-sync python backtest/offline_manifest.py verify --manifest "
              "backtest/candidates/stochastic_momentum_index_1663_manifest.json`",
              "- `uv run --no-sync python backtest/candidates/stochastic_momentum_index_1663/run_study.py --protocol-hash`",
              "- `uv run --no-sync python backtest/candidates/stochastic_momentum_index_1663/run_study.py`",
              "- `uv run --no-sync python backtest/candidates/stochastic_momentum_index_1663/run_parity.py`",
              "- `uv run --no-sync python backtest/candidates/stochastic_momentum_index_1663/run_study.py --render-only`",
              "", "## Source hashes", ""]
    for path, digest in sorted(result["source_sha256"].items()):
        lines.append(f"- `{path}` `{digest}`")
    lines += ["", "---", "Created with LLM: Opus 5.5 | high | Harness: Claude Code", ""]
    return "\n".join(lines)


def main(argv=None) -> int:
    p = argparse.ArgumentParser(description="#1663 frozen study driver (suggest-only, offline)")
    p.add_argument("--json", default=RESULTS_PATH, dest="json_out")
    p.add_argument("--report", default=REPORT_PATH)
    p.add_argument("--workers", type=int, default=max(1, min(8, (os.cpu_count() or 2) - 1)))
    p.add_argument("--render-only", action="store_true", help="Re-render the report from the results JSON")
    p.add_argument("--protocol-hash", action="store_true", help="Print the frozen protocol sha256 and exit")
    args = p.parse_args(argv)
    if args.protocol_hash:
        print(protocol_sha256())
        return 0
    if args.render_only:
        with open(args.json_out) as fh:
            result = json.load(fh)
    else:
        result = run(args.workers)
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

#!/usr/bin/env python3

import argparse
import copy
import hashlib
import json
import os
import statistics
import subprocess
import sys
import tempfile
from types import SimpleNamespace

import pandas as pd

_HERE = os.path.dirname(os.path.abspath(__file__))
_BACKTEST = os.path.abspath(os.path.join(_HERE, "..", ".."))
_REPO = os.path.abspath(os.path.join(_BACKTEST, ".."))
sys.path.insert(0, _BACKTEST)
sys.path.insert(0, os.path.join(_REPO, "shared_tools"))

CACHE_ENV = "GO_TRADER_OHLCV_CACHE_DB"
ISSUE = 1643
TRACKING_GROUP = "hyperliquid-strategy-candidates-2026-10-02"
MANIFEST = os.path.join(_HERE, "study_manifest.json")
CANDIDATE_SEED = os.path.join(_HERE, "candidate_seed.json")
PROXY_FREEZE = os.path.join(_HERE, "proxy_freeze.json")
CANDIDATE = "aroon_trend"
COMPARATORS = ("adx_trend", "breakout")
CAPITAL = 1000.0
PROXY_SELECTION_WINDOW = "is"
PROXY_PRIMARY_WINDOW = "oos"
PROXY_HELD_OUT_WINDOWS = ("oos", "2023", "2024", "2025H1")
PROXY_SECONDARY_WINDOWS = ("2023", "2024", "2025H1")
PROXY_OOS_END = "2026-10-01"
PROXY_FETCH_SINCE = "2022-12-01"
PROXY_EXCHANGE = "binanceus"
PROXY_EXPECTED_WINDOWS = {
    "is": ("2025-06-10", "2026-01-01"),
    "oos": ("2026-01-01", None),
    "2023": ("2023-01-01", "2024-01-01"),
    "2024": ("2024-01-01", "2025-01-01"),
    "2025H1": ("2025-01-01", "2025-07-01"),
}
VENUE_SELECTION_WINDOW = "train"
VENUE_HELD_OUT_WINDOW = "test"
BASE_COST = 1.0
STRESS_COST = 2.0
MIN_TRAIN_POSITIONS = 10
MIN_HELD_OUT_CLUSTERS = 30
MIN_SECONDARY_M1_PASSES = 2
PARITY_RUNS = ((None, False), (200, False), (200, True))
CLI_PARITY_SYMBOL = "BTC/USDT"
CLI_PARITY_TIMEFRAME = "4h"
CLI_PARITY_SINCE = "2026-01-01"
CLI_PARITY_RUNS = (("--window", "0"), ("--window", "200"), ("--window", "200", "--batched"))
EDGE_SOURCE_BY_OUTCOME = {"pass": "unvalidated", "fail": "study_fail", "inconclusive": "study_inconclusive"}
SOURCE_FILES = (
    "shared_strategies/open/aroon_trend.py",
    "shared_strategies/open/registry.py",
    "shared_tools/strategy_composition.py",
    "shared_tools/funding_fetcher.py",
    "shared_tools/data_fetcher.py",
    "shared_strategies/close/tiered_tp_pct.py",
    "shared_scripts/check_hyperliquid.py",
    "backtest/backtester.py",
    "backtest/eval_windows.py",
    "backtest/offline_manifest.py",
    "backtest/optimizer.py",
    "backtest/parity_diff.py",
    "backtest/candidates/aroon_trend_1643/run_study.py",
    "backtest/candidates/aroon_trend_1643/candidate_seed.json",
)
SELECTION_RULE = (
    "candidate only, direction both, the seed close and stop, scored on the selection window "
    "(proxy: M1 `is`; venue: manifest `train`): eligible = every dataset scored, no incomplete funded leg, "
    f"no liquidated leg, venue candle and funding coverage complete, >= {MIN_TRAIN_POSITIONS} positions; "
    "pick max mean Sharpe, then max mean drawdown-adjusted return, then earliest optimizer-grid order; "
    "no eligible combination = inconclusive")
PROXY_VERDICT_RULE = (
    "inconclusive when any held-out window (oos, 2023, 2024, 2025H1) has a missing dataset, an incomplete "
    "funded leg (candidate, comparator or M1 incumbent), a funding row fetched during scoring or a frame "
    "whose hash differs from the freeze; when no `is` combination is eligible; or when the candidate has "
    f"fewer than {MIN_HELD_OUT_CLUSTERS} independent position clusters in oos. Otherwise pass only if the "
    f"contract-aware M1 verdict is pass in oos and in at least {MIN_SECONDARY_M1_PASSES} of 2023, 2024 and "
    "2025H1, the candidate oos mean return is positive, and the candidate oos mean Sharpe beats adx_trend "
    "and breakout under the same contract; else fail")
VENUE_VERDICT_RULE = (
    "inconclusive when venue candle or funding coverage is incomplete, any funded leg is incomplete, "
    "the provenance is not venue data, no `train` combination is eligible, or the candidate has fewer than "
    f"{MIN_HELD_OUT_CLUSTERS} independent position clusters in `test`. Otherwise pass only if the candidate "
    "mean return is positive at cost x1 and x2, the contract-aware M1 verdict on `test` at cost x1 is pass, "
    "and the candidate mean Sharpe beats adx_trend and breakout at cost x1; else fail")
OVERALL_RULE = (
    "pass only if the proxy and venue conclusions both pass; fail if either is fail; otherwise inconclusive. "
    "The registry keeps edge_status no_edge in every outcome")


def _sha_file(path: str) -> str:
    h = hashlib.sha256()
    with open(path, "rb") as fh:
        for chunk in iter(lambda: fh.read(1 << 20), b""):
            h.update(chunk)
    return h.hexdigest()


def _load_json(path: str) -> dict:
    with open(path) as fh:
        return json.load(fh)


def contract_of(seed: dict) -> dict:
    return {k: copy.deepcopy(seed[k]) for k in
            ("direction", "close_strategies", "stop_loss_atr_mult", "comparison_mode")}


def load_seed() -> dict:
    seed = _load_json(CANDIDATE_SEED)
    if seed["name"] != CANDIDATE or seed["direction"] != "both":
        raise SystemExit("candidate seed must name aroon_trend with direction both")
    closes = seed["close_strategies"]
    if len(closes) != 1 or closes[0]["name"] != "tiered_tp_pct":
        raise SystemExit("candidate seed must declare exactly one tiered_tp_pct close")
    if seed.get("trailing_stop_atr_mult") or not seed.get("stop_loss_atr_mult"):
        raise SystemExit("candidate seed must declare exactly one fixed entry ATR stop")
    return seed


def proxy_windows() -> dict:
    import eval_windows as ew
    if dict(ew.WINDOWS) != PROXY_EXPECTED_WINDOWS:
        raise SystemExit(f"M1 windows changed since the freeze: {ew.WINDOWS}")
    out = dict(ew.WINDOWS)
    out["oos"] = (out["oos"][0], PROXY_OOS_END)
    return out


def proxy_datasets() -> list:
    import eval_windows as ew
    return [tuple(d) for d in ew.DATASETS]


def protocol(seed: dict) -> dict:
    from optimizer import DEFAULT_PARAM_RANGES
    return {
        "issue": ISSUE,
        "tracking_group": TRACKING_GROUP,
        "candidate_seed": seed,
        "comparators": list(COMPARATORS),
        "comparison_contract": contract_of(seed),
        "candidate_grid": {k: list(v) for k, v in DEFAULT_PARAM_RANGES[CANDIDATE].items()},
        "capital": CAPITAL,
        "proxy": {
            "registry": "futures",
            "exchange": PROXY_EXCHANGE,
            "datasets": [f"{s} {t}" for s, t in proxy_datasets()],
            "windows": {k: list(v) for k, v in proxy_windows().items()},
            "selection_window": PROXY_SELECTION_WINDOW,
            "primary_window": PROXY_PRIMARY_WINDOW,
            "held_out_windows": list(PROXY_HELD_OUT_WINDOWS),
            "funding": "charge (Hyperliquid hourly funding from the frozen isolated cache); "
                       "off is a labelled sensitivity only",
            "verdict_rule": PROXY_VERDICT_RULE,
        },
        "venue": {
            "manifest": os.path.relpath(MANIFEST, _REPO),
            "selection_window": VENUE_SELECTION_WINDOW,
            "held_out_window": VENUE_HELD_OUT_WINDOW,
            "cost_multipliers": {"base": BASE_COST, "stress": STRESS_COST},
            "verdict_rule": VENUE_VERDICT_RULE,
        },
        "selection_rule": SELECTION_RULE,
        "overall_rule": OVERALL_RULE,
        "min_train_positions": MIN_TRAIN_POSITIONS,
        "min_held_out_clusters": MIN_HELD_OUT_CLUSTERS,
        "min_secondary_m1_passes": MIN_SECONDARY_M1_PASSES,
        "parity_runs": [{"window": w, "batched": b} for w, b in PARITY_RUNS],
        "cli_parity": {"symbol": CLI_PARITY_SYMBOL, "timeframe": CLI_PARITY_TIMEFRAME,
                       "since": CLI_PARITY_SINCE, "runs": [list(r) for r in CLI_PARITY_RUNS],
                       "mode": "ad-hoc refs on the proxy cache, network blocked; batched slots carry explicit paper mode"},
        "intrabar_resolution": "ohlc_walk",
    }


def protocol_sha256(proto: dict) -> str:
    return hashlib.sha256(json.dumps(proto, sort_keys=True, separators=(",", ":")).encode()).hexdigest()


def frame_sha256(frame: pd.DataFrame) -> str:
    cols = ["open", "high", "low", "close", "volume"]
    body = frame[cols].copy()
    body.insert(0, "ts", [str(t) for t in frame.index])
    return hashlib.sha256(body.to_csv(index=False).encode()).hexdigest()


def _proxy_frame(symbol: str, timeframe: str, window: tuple) -> pd.DataFrame:
    from data_fetcher import load_cached_data
    start, end = window
    df = load_cached_data(symbol, timeframe, start_date=start, end_date=end)
    if end is not None and not df.empty:
        df = df[df.index < pd.Timestamp(end)]
    return df


def _frame_inventory(df: pd.DataFrame, timeframe: str, window: tuple) -> dict:
    step = pd.Timedelta(timeframe)
    start, end = pd.Timestamp(window[0]), pd.Timestamp(window[1])
    expected = int((end - start) / step)
    gaps = (df.index.to_series().diff().dropna() / step - 1) if len(df) > 1 else pd.Series(dtype=float)
    return {
        "rows": int(len(df)),
        "expected_rows": expected,
        "first": str(df.index[0]) if len(df) else None,
        "last": str(df.index[-1]) if len(df) else None,
        "max_gap_bars": int(gaps.max()) if len(gaps) else 0,
        "missing_rows": int(expected - len(df)),
        "sha256": frame_sha256(df) if len(df) else None,
    }


def _funding_inventory(coin: str, start: str, end: str) -> dict:
    from funding_fetcher import funding_row_sha256
    from storage import load_funding_coverage, load_funding_first_ts, load_funding_rates
    s = int(pd.Timestamp(start, tz="UTC").timestamp() * 1000)
    e = int(pd.Timestamp(end, tz="UTC").timestamp() * 1000)
    rows = load_funding_rates("hyperliquid", coin, s, e)
    return {
        "rows": int(len(rows)),
        "first_record": (str(pd.to_datetime(int(load_funding_first_ts("hyperliquid", coin)), unit="ms"))
                         if load_funding_first_ts("hyperliquid", coin) is not None else None),
        "coverage": [[int(a), int(b)] for a, b in load_funding_coverage("hyperliquid", coin)],
        "sha256": funding_row_sha256(rows["timestamp"].tolist(), rows["rate"].tolist()) if len(rows) else None,
    }


def throttled_hl_adapter(pause_s: float = 1.0, retries: int = 8):
    import time
    import funding_fetcher
    adapter = funding_fetcher._hl_adapter()
    raw = adapter._info.funding_history

    def paced(coin, start, end=None):
        for attempt in range(retries):
            time.sleep(pause_s * (1 if attempt == 0 else 2 ** attempt))
            try:
                return raw(coin, start) if end is None else raw(coin, start, end)
            except Exception as exc:
                if "429" not in str(exc) or attempt == retries - 1:
                    raise
        raise RuntimeError("unreachable")

    adapter._info.funding_history = paced
    return adapter


def freeze_proxy(cache_db: str) -> dict:
    from data_fetcher import fetch_full_history
    from funding_fetcher import load_cached_funding
    windows = proxy_windows()
    datasets = proxy_datasets()
    for symbol, timeframe in datasets:
        if _proxy_frame_or_empty(symbol, timeframe, (PROXY_FETCH_SINCE, PROXY_OOS_END)).empty:
            fetch_full_history(symbol, timeframe, PROXY_FETCH_SINCE, PROXY_EXCHANGE, store=True, limit=1000)
    coins = sorted({s.split("/")[0] for s, _ in datasets})
    adapter = throttled_hl_adapter()
    for coin in coins:
        box = {}
        load_cached_funding(coin, PROXY_FETCH_SINCE, end_date=PROXY_OOS_END, adapter=adapter, source_box=box)
        if box.get("fetch_error"):
            raise SystemExit(f"funding freeze for {coin} failed: {box['fetch_error'][:400]}")
    frames = {}
    for symbol, timeframe in datasets:
        for wname, window in windows.items():
            frames[f"{symbol} {timeframe} {wname}"] = _frame_inventory(
                _proxy_frame(symbol, timeframe, window), timeframe, window)
    funding = {coin: _funding_inventory(coin, PROXY_FETCH_SINCE, PROXY_OOS_END) for coin in coins}
    return {
        "exchange": PROXY_EXCHANGE,
        "fetch_since": PROXY_FETCH_SINCE,
        "oos_end": PROXY_OOS_END,
        "cache_db_name": os.path.basename(cache_db),
        "frames": frames,
        "funding": funding,
        "note": ("Binance US USDT candles are a price proxy for Hyperliquid perpetuals. The cache is an isolated "
                 "SQLite file outside the repository; replay refetches it with --freeze-proxy and the run "
                 "refuses any frame or funding digest that differs from this file."),
    }


def _proxy_frame_or_empty(symbol, timeframe, window):
    from storage import load_ohlcv
    s = int(pd.Timestamp(window[0]).timestamp() * 1000)
    e = int(pd.Timestamp(window[1]).timestamp() * 1000)
    return load_ohlcv(PROXY_EXCHANGE, symbol, timeframe, s, e)


def verify_proxy_freeze(freeze: dict) -> dict:
    windows = proxy_windows()
    problems = []
    for symbol, timeframe in proxy_datasets():
        for wname, window in windows.items():
            key = f"{symbol} {timeframe} {wname}"
            got = _frame_inventory(_proxy_frame(symbol, timeframe, window), timeframe, window)
            if got != freeze["frames"].get(key):
                problems.append(f"frame {key}: {got.get('sha256')} != {freeze['frames'].get(key, {}).get('sha256')}")
    for coin, want in freeze["funding"].items():
        got = _funding_inventory(coin, freeze["fetch_since"], freeze["oos_end"])
        if got != want:
            problems.append(f"funding {coin}: {got.get('sha256')} != {want.get('sha256')}")
    if problems:
        raise SystemExit("proxy inputs differ from proxy_freeze.json:\n  " + "\n  ".join(problems))
    return {"verified_frames": len(freeze["frames"]), "verified_funding": sorted(freeze["funding"])}


class _BlockedFetch(RuntimeError):
    pass


def block_network() -> None:
    import data_fetcher
    import funding_fetcher

    def _refuse(*_a, **_k):
        raise _BlockedFetch("network fetch blocked while scoring the frozen study")

    data_fetcher.fetch_full_history = _refuse
    data_fetcher.fetch_ohlcv = _refuse
    funding_fetcher._hl_adapter = _refuse


def position_clusters(positions: list) -> int:
    spans = sorted((pd.Timestamp(p["entry_date"]), pd.Timestamp(p["exit_date"])) for p in positions)
    count, end = 0, None
    for s, e in spans:
        if end is None or s > end:
            count += 1
            end = e
        else:
            end = max(end, e)
    return count


def _funding_brief(leg: dict) -> dict:
    block = leg.get("funding") or {}
    if not block and leg.get("manifest"):
        cov = leg["manifest"]["funding_coverage"]
        return {
            "mode": "charge (manifest)",
            "source": "manifest",
            "complete": cov.get("complete"),
            "sha256": leg["manifest"].get("funding_sha256"),
            "venue_absent_sha256": None,
            "expected_hours": cov.get("expected_hours"),
            "present_hours": cov.get("present_hours"),
            "missing_hours": cov.get("missing_hours"),
            "venue_absent_hours": 0,
            "unpriced_held_hours": 0 if cov.get("complete") else None,
            "fetch_error": None,
        }
    cov = block.get("coverage") or {}
    return {
        "mode": block.get("mode"),
        "source": block.get("source"),
        "complete": block.get("complete"),
        "sha256": block.get("sha256"),
        "venue_absent_sha256": block.get("venue_absent_sha256"),
        "expected_hours": cov.get("expected_hours"),
        "present_hours": cov.get("present_hours"),
        "missing_hours": cov.get("missing_hours"),
        "venue_absent_hours": cov.get("venue_absent_hours"),
        "unpriced_held_hours": block.get("unpriced_held_hours"),
        "fetch_error": block.get("fetch_error") or block.get("error"),
    }


def summarize(score: dict, venue: bool) -> dict:
    rows = [r for r in score["rows"] if r["leg"] is not None]
    out = {"datasets": len(rows), "incumbent_m1_verdict": score.get("verdict"),
           "m1_mean_sharpe": score.get("mean_sharpe"), "m1_bar_sharpe": score.get("mean_bar_sharpe"),
           "m1_mean_ddadj": score.get("mean_ddadj"), "m1_bar_ddadj": score.get("mean_bar_ddadj")}
    if not rows:
        out.update({"positions": 0, "clusters": 0, "funding_incomplete_legs": 0, "liquidated_legs": 0,
                    "coverage_complete": False, "funding_fetched_legs": 0})
        return out
    legs = [r["leg"] for r in rows]
    positions, per_dataset = [], {}
    for r in rows:
        leg, ex = r["leg"], r["leg"]["execution"]
        item = {
            "return_pct": leg["return_pct"], "sharpe": leg["sharpe"], "max_dd_pct": leg["max_dd_pct"],
            "ddadj": leg["ddadj"], "net_pnl_usd": ex["net_pnl_usd"], "fees_usd": ex["fees_usd"],
            "funding_pnl_usd": ex["funding_pnl_usd"], "turnover": ex["turnover"], "positions": ex["positions"],
            "long_positions": ex["long_positions"], "short_positions": ex["short_positions"],
            "bars_in_market": ex["bars_in_market"], "funding_incomplete": bool(leg.get("funding_incomplete")),
            "funding": _funding_brief(leg), "beats_incumbent_sharpe": r["beats_sharpe"],
            "beats_incumbent_ddadj": r["beats_ddadj"],
            "min_entry_notional_usd": (round(min(p["entry_notional"] for p in ex["position_list"]), 4)
                                       if ex["position_list"] else None),
        }
        if venue:
            item["candle_coverage"] = leg["manifest"]["candle_coverage"]
            item["funding_coverage"] = leg["manifest"]["funding_coverage"]
            item["rejected_entries"] = ex.get("rejected_entries", 0)
        per_dataset[r["dataset"]] = item
        for p in ex["position_list"]:
            positions.append(dict(p, dataset=r["dataset"]))
    abs_ds = [abs(v["net_pnl_usd"]) for v in per_dataset.values()]
    abs_pos = sorted((abs(p["net_pnl"]) for p in positions), reverse=True)
    bars = score.get("bars") or {}
    out.update({
        "mean_return_pct": round(statistics.mean(l["return_pct"] for l in legs), 4),
        "mean_sharpe": round(statistics.mean(l["sharpe"] for l in legs), 4),
        "mean_ddadj": round(statistics.mean(l["ddadj"] for l in legs), 4),
        "worst_max_dd_pct": round(min(l["max_dd_pct"] for l in legs), 4),
        "net_pnl_usd": round(sum(v["net_pnl_usd"] for v in per_dataset.values()), 4),
        "fees_usd": round(sum(v["fees_usd"] for v in per_dataset.values()), 4),
        "funding_pnl_usd": round(sum(v["funding_pnl_usd"] for v in per_dataset.values()), 4),
        "mean_turnover": round(statistics.mean(v["turnover"] for v in per_dataset.values()), 4),
        "positions": len(positions),
        "long_positions": sum(1 for p in positions if p["side"] == "long"),
        "short_positions": sum(1 for p in positions if p["side"] == "short"),
        "clusters": position_clusters(positions),
        "liquidated_legs": sum(1 for l in legs if l.get("liquidated")),
        "funding_incomplete_legs": sum(1 for v in per_dataset.values() if v["funding_incomplete"]),
        "funding_fetched_legs": sum(1 for v in per_dataset.values() if v["funding"]["source"] == "fetched"),
        "funding_unpriced_held_hours": sum(int(v["funding"]["unpriced_held_hours"] or 0)
                                           for v in per_dataset.values()),
        "incumbent_funding_incomplete": sum(1 for b in bars.values() if b and b.get("funding_incomplete")),
        "top_dataset_abs_pnl_share": (round(max(abs_ds) / sum(abs_ds), 4) if sum(abs_ds) > 0 else None),
        "top5_position_abs_pnl_share": (round(sum(abs_pos[:5]) / sum(abs_pos), 4) if sum(abs_pos) > 0 else None),
        "min_entry_notional_usd": min((v["min_entry_notional_usd"] for v in per_dataset.values()
                                       if v["min_entry_notional_usd"] is not None), default=None),
        "coverage_complete": (all(v["candle_coverage"]["complete"] and v["funding_coverage"]["complete"]
                                  for v in per_dataset.values()) if venue else None),
        "per_dataset": per_dataset,
        "positions_list": positions,
        "close_validation": score.get("close_validation"),
    })
    return out


def _brief(summary: dict, drop=("per_dataset", "positions_list")) -> dict:
    return {k: v for k, v in summary.items() if k not in drop}


def eligible(summary: dict, expected: int, venue: bool) -> bool:
    ok = (summary["datasets"] == expected and summary["funding_incomplete_legs"] == 0
          and summary["liquidated_legs"] == 0 and summary["positions"] >= MIN_TRAIN_POSITIONS
          and summary["funding_fetched_legs"] == 0)
    if venue:
        ok = ok and bool(summary["coverage_complete"])
    return ok


def select(rows: list, expected: int, venue: bool) -> dict:
    ok = [(i, r) for i, r in enumerate(rows) if eligible(r["summary"], expected, venue)]
    if not ok:
        return {"rule_outcome": "no_eligible_combo", "selected": None, "eligible": 0}
    best = max(ok, key=lambda kv: (kv[1]["summary"]["mean_sharpe"], kv[1]["summary"]["mean_ddadj"], -kv[0]))[1]
    return {"rule_outcome": "selected", "selected": best["label"], "params": best["params"], "eligible": len(ok)}


def neighbors(params: dict, rows: list) -> dict:
    from optimizer import DEFAULT_PARAM_RANGES
    ranges = DEFAULT_PARAM_RANGES[CANDIDATE]
    near = []
    for r in rows:
        diffs = [abs(list(grid).index(params[k]) - list(grid).index(r["params"][k])) for k, grid in ranges.items()]
        if sum(diffs) == 1:
            near.append(r["summary"]["mean_sharpe"])
    return {"neighbor_count": len(near),
            "neighbor_median_sharpe": round(statistics.median(near), 4) if near else None,
            "neighbor_min_sharpe": round(min(near), 4) if near else None,
            "neighbor_positive_sharpe": sum(1 for v in near if v > 0)}


def _rank(values: list) -> list:
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


def spearman(a: list, b: list):
    if len(a) < 3:
        return None
    ra, rb = _rank(a), _rank(b)
    ma, mb = statistics.mean(ra), statistics.mean(rb)
    num = sum((x - ma) * (y - mb) for x, y in zip(ra, rb))
    den = (sum((x - ma) ** 2 for x in ra) * sum((y - mb) ** 2 for y in rb)) ** 0.5
    return round(num / den, 4) if den else None


def arm_spec(seed: dict, name: str, params: dict) -> dict:
    spec = contract_of(seed)
    spec.update({"name": name, "params": dict(params)})
    return spec


def _grid(defaults: dict) -> list:
    from eval_windows import expand_sweep
    from optimizer import DEFAULT_PARAM_RANGES
    specs = [(k, list(v)) for k, v in DEFAULT_PARAM_RANGES[CANDIDATE].items()]
    return expand_sweep(dict(defaults), specs)


def proxy_evidence_ok(arms_by_window: dict) -> tuple:
    reasons = []
    for wname, arms in sorted(arms_by_window.items()):
        for arm, s in sorted(arms.items()):
            if s["datasets"] != len(proxy_datasets()):
                reasons.append(f"{wname} {arm}: {s['datasets']} datasets scored")
            if s["funding_incomplete_legs"]:
                reasons.append(f"{wname} {arm}: {s['funding_incomplete_legs']} incomplete funded legs")
            if s.get("funding_fetched_legs"):
                reasons.append(f"{wname} {arm}: funding fetched during scoring")
            if s.get("incumbent_funding_incomplete"):
                reasons.append(f"{wname} {arm}: {s['incumbent_funding_incomplete']} incumbent datasets with incomplete funding")
    return (not reasons), reasons


def proxy_verdict(held: dict, selection: dict) -> dict:
    ok, reasons = proxy_evidence_ok(held)
    oos = held[PROXY_PRIMARY_WINDOW]
    cand = oos["candidate"]
    secondary_passes = sum(1 for w in PROXY_SECONDARY_WINDOWS if held[w]["candidate"]["incumbent_m1_verdict"] == "pass")
    checks = {
        "evidence_complete": ok,
        "selection_eligible": selection.get("selected") is not None,
        "sample_floor_met": cand.get("clusters", 0) >= MIN_HELD_OUT_CLUSTERS,
        "m1_oos_pass": cand["incumbent_m1_verdict"] == "pass",
        "m1_secondary_passes": secondary_passes >= MIN_SECONDARY_M1_PASSES,
        "oos_net_positive": cand.get("mean_return_pct", 0.0) > 0,
        "oos_beats_adx_trend_sharpe": cand.get("mean_sharpe", 0.0) > oos["adx_trend"].get("mean_sharpe", 0.0),
        "oos_beats_breakout_sharpe": cand.get("mean_sharpe", 0.0) > oos["breakout"].get("mean_sharpe", 0.0),
    }
    if not ok:
        outcome, reason = "inconclusive", "held-out evidence is incomplete: " + "; ".join(reasons)
    elif not checks["selection_eligible"]:
        outcome, reason = "inconclusive", "no `is` combination met the frozen eligibility rule"
    elif not checks["sample_floor_met"]:
        outcome, reason = "inconclusive", (
            f"oos independent position clusters {cand.get('clusters', 0)} below {MIN_HELD_OUT_CLUSTERS}")
    elif all(checks.values()):
        outcome, reason = "pass", "every frozen proxy check passed"
    else:
        outcome, reason = "fail", "failed frozen checks: " + ", ".join(sorted(k for k, v in checks.items() if not v))
    return {"outcome": outcome, "reason": reason, "checks": checks, "secondary_m1_passes": secondary_passes}


def venue_verdict(held: dict, selection: dict, provenance_kind: str) -> dict:
    base = {k: v["base"] for k, v in held.items()}
    stress = {k: v["stress"] for k, v in held.items()}
    evidence_ok = (provenance_kind == "venue"
                   and all(s["coverage_complete"] and s["funding_incomplete_legs"] == 0
                           and not s.get("incumbent_funding_incomplete")
                           for arm in held.values() for s in arm.values()))
    cand = base["candidate"]
    checks = {
        "evidence_complete": evidence_ok,
        "selection_eligible": selection.get("selected") is not None,
        "sample_floor_met": cand.get("clusters", 0) >= MIN_HELD_OUT_CLUSTERS,
        "net_positive_x1": cand.get("mean_return_pct", 0.0) > 0,
        "net_positive_x2": stress["candidate"].get("mean_return_pct", 0.0) > 0,
        "m1_test_pass_x1": cand["incumbent_m1_verdict"] == "pass",
        "beats_adx_trend_sharpe_x1": cand.get("mean_sharpe", 0.0) > base["adx_trend"].get("mean_sharpe", 0.0),
        "beats_breakout_sharpe_x1": cand.get("mean_sharpe", 0.0) > base["breakout"].get("mean_sharpe", 0.0),
    }
    if not evidence_ok:
        outcome, reason = "inconclusive", "venue candle, funding or provenance evidence is incomplete"
    elif not checks["selection_eligible"]:
        outcome, reason = "inconclusive", "no `train` combination met the frozen eligibility rule"
    elif not checks["sample_floor_met"]:
        outcome, reason = "inconclusive", (
            f"test independent position clusters {cand.get('clusters', 0)} below {MIN_HELD_OUT_CLUSTERS}")
    elif all(checks.values()):
        outcome, reason = "pass", "every frozen venue check passed"
    else:
        outcome, reason = "fail", "failed frozen checks: " + ", ".join(sorted(k for k, v in checks.items() if not v))
    return {"outcome": outcome, "reason": reason, "checks": checks}


def overall_verdict(proxy: dict, venue: dict) -> dict:
    p, v = proxy["outcome"], venue["outcome"]
    if p == "pass" and v == "pass":
        outcome = "pass"
    elif "fail" in (p, v):
        outcome = "fail"
    else:
        outcome = "inconclusive"
    return {"outcome": outcome, "proxy": p, "venue": v, "edge_source": EDGE_SOURCE_BY_OUTCOME[outcome],
            "availability": ("edge_status no_edge in every outcome: hidden from discovery, explicit paper mode "
                             "runs it, live use still needs allow_no_edge: true (the repository's acknowledged-live "
                             "exception); a research pass approves nothing and promotion is a separate reviewed "
                             "action")}


def verdict_guards(proxy_held: dict, proxy_sel: dict, venue_held: dict, venue_sel: dict) -> dict:
    out = {}
    g = copy.deepcopy(proxy_held)
    g["oos"]["candidate"]["funding_incomplete_legs"] = 1
    out["proxy_funding_incomplete"] = proxy_verdict(g, proxy_sel)["outcome"]
    g = copy.deepcopy(proxy_held)
    g["2024"]["breakout"]["datasets"] = len(proxy_datasets()) - 1
    out["proxy_missing_dataset"] = proxy_verdict(g, proxy_sel)["outcome"]
    g = copy.deepcopy(proxy_held)
    g["oos"]["candidate"]["clusters"] = MIN_HELD_OUT_CLUSTERS - 1
    out["proxy_below_sample_floor"] = proxy_verdict(_complete_proxy(g), proxy_sel)["outcome"]
    out["proxy_no_selection"] = proxy_verdict(_complete_proxy(copy.deepcopy(proxy_held)), {"selected": None})["outcome"]
    g = copy.deepcopy(venue_held)
    g["candidate"]["base"]["funding_incomplete_legs"] = 1
    out["venue_funding_incomplete"] = venue_verdict(g, venue_sel, "venue")["outcome"]
    g = copy.deepcopy(venue_held)
    g["adx_trend"]["stress"]["coverage_complete"] = False
    out["venue_coverage_incomplete"] = venue_verdict(g, venue_sel, "venue")["outcome"]
    out["venue_proxy_provenance"] = venue_verdict(venue_held, venue_sel, "proxy")["outcome"]
    g = copy.deepcopy(venue_held)
    g["candidate"]["base"]["clusters"] = MIN_HELD_OUT_CLUSTERS - 1
    out["venue_below_sample_floor"] = venue_verdict(_complete_venue(g), venue_sel, "venue")["outcome"]
    out["venue_no_selection"] = venue_verdict(_complete_venue(copy.deepcopy(venue_held)), {"selected": None}, "venue")["outcome"]
    if any(v == "pass" for v in out.values()):
        raise SystemExit(f"verdict guard failed: {out}")
    return out


def _complete_proxy(held: dict) -> dict:
    for arms in held.values():
        for s in arms.values():
            s["datasets"] = len(proxy_datasets())
            s["funding_incomplete_legs"] = 0
            s["funding_fetched_legs"] = 0
            s["incumbent_funding_incomplete"] = 0
    return held


def _complete_venue(held: dict) -> dict:
    for arm in held.values():
        for s in arm.values():
            s["coverage_complete"] = True
            s["funding_incomplete_legs"] = 0
            s["incumbent_funding_incomplete"] = 0
    return held


def _ms(value):
    return None if value != value else str(pd.to_datetime(int(value), unit="ms"))


def entry_audit(reg, frames: dict, params: dict, positions: list) -> list:
    outs = {k: reg.apply_strategy(CANDIDATE, f, dict(params)) for k, f in frames.items()}
    span = int(params["period"]) + 2
    rows = []
    for p in sorted(positions, key=lambda x: (x["dataset"], x["entry_date"])):
        frame, out = frames[p["dataset"]], outs[p["dataset"]]
        fill = out.index.get_loc(pd.Timestamp(p["entry_date"]))
        sig_row = fill - 1
        rec = out.iloc[sig_row]
        want = 1 if p["side"] == "long" else -1
        if int(rec["signal"]) != want:
            raise SystemExit(f"entry audit: {p['dataset']} {p['entry_date']} fill has no {p['side']} signal on the prior bar")
        window = frame.iloc[sig_row - span + 1:sig_row + 1]
        rows.append({
            "dataset": p["dataset"],
            "side": p["side"],
            "signal_bar": str(out.index[sig_row]),
            "fill_bar": str(out.index[fill]),
            "exit_bar": p["exit_date"],
            "exit_reasons": p.get("exit_reasons"),
            "net_pnl_usd": p["net_pnl"],
            "entry_notional_usd": round(p["entry_notional"], 6),
            "params": dict(params),
            "reason": rec["aroon_reason"],
            "input_first_bar": str(window.index[0]),
            "input_bars": int(len(window)),
            "input_sha256": frame_sha256(window),
            "up": float(rec["aroon_up"]),
            "down": float(rec["aroon_down"]),
            "prev_up": float(rec["aroon_prev_up"]),
            "prev_down": float(rec["aroon_prev_down"]),
            "high_age": int(rec["aroon_high_age"]),
            "low_age": int(rec["aroon_low_age"]),
        })
    return rows


def frozen_parity(frames: dict, seed: dict, params: dict, timeframe_of: dict, coin_of: dict) -> list:
    from parity_diff import ParityConfig, compute_parity_frame
    from parity_diff import summarize as parity_summarize
    rows = []
    for key, frame in sorted(frames.items()):
        for window, batched in PARITY_RUNS:
            if batched:
                cfg = ParityConfig(strategy_name=CANDIDATE, params=dict(params), registry="futures",
                                   batched=True, symbol=coin_of[key], timeframe=timeframe_of[key])
            else:
                cfg = ParityConfig(strategy_name=CANDIDATE, params=dict(params), registry="futures",
                                   close_refs=copy.deepcopy(seed["close_strategies"]), direction="both",
                                   comparison_mode=seed["comparison_mode"])
            parity = compute_parity_frame(frame, cfg=cfg, window=window)
            summary = parity_summarize(parity)
            rows.append({
                "dataset": key,
                "window": "every prefix" if window is None else window,
                "batched": batched,
                "input_sha256": frame_sha256(frame),
                "frame_bars": len(frame),
                "compared_bars": int(len(parity)),
                "mismatches": int((~parity["match"]).sum()),
                "batch_mismatches": summary.get("batch_mismatches"),
                "entry_decisions": int((parity["live_open_action"] != "none").sum()),
                "decision_agreement": summary["decision_agreement"],
                "close_parity": summary.get("close_parity"),
                "clean": summary.get("clean"),
            })
    return rows


CLI_WRAPPER = (
    "import runpy, sys\n"
    "sys.path.insert(0, sys.argv[1]); sys.path.insert(0, sys.argv[2])\n"
    "import data_fetcher, funding_fetcher\n"
    "def _refuse(*a, **k):\n"
    "    raise RuntimeError('network fetch blocked while checking the frozen study')\n"
    "data_fetcher.fetch_full_history = _refuse\n"
    "data_fetcher.fetch_ohlcv = _refuse\n"
    "funding_fetcher._hl_adapter = _refuse\n"
    "script = sys.argv[3]\n"
    "sys.argv = [script] + sys.argv[4:]\n"
    "runpy.run_path(script, run_name='__main__')\n"
)


def cli_parity_args(seed: dict, params: dict) -> list:
    close = seed["close_strategies"][0]
    return ["--strategy", CANDIDATE, "--registry", "futures", "--platform", "hyperliquid",
            "--symbol", CLI_PARITY_SYMBOL, "--timeframe", CLI_PARITY_TIMEFRAME, "--since", CLI_PARITY_SINCE,
            "--params", json.dumps(params, sort_keys=True),
            "--close", close["name"] + ":" + json.dumps(close["params"], sort_keys=True),
            "--comparison-mode", seed["comparison_mode"]]


def cli_parity(seed: dict, params: dict, cache_db: str) -> dict:
    from data_fetcher import load_cached_data
    frame = load_cached_data(CLI_PARITY_SYMBOL, CLI_PARITY_TIMEFRAME, start_date=CLI_PARITY_SINCE)
    base = cli_parity_args(seed, params)
    rows = []
    with tempfile.TemporaryDirectory() as tmp:
        for extra in CLI_PARITY_RUNS:
            jsonl = os.path.join(tmp, "parity.jsonl")
            argv = [sys.executable, "-c", CLI_WRAPPER, _BACKTEST, os.path.join(_REPO, "shared_tools"),
                    os.path.join(_BACKTEST, "parity_diff.py"), *base, "--jsonl", jsonl, *extra]
            env = dict(os.environ, **{CACHE_ENV: cache_db})
            proc = subprocess.run(argv, cwd=_REPO, env=env, capture_output=True, text=True, timeout=14400)
            bars = mismatches = batch = entries = 0
            if os.path.exists(jsonl):
                with open(jsonl) as fh:
                    for line in fh:
                        rec = json.loads(line)
                        if "match" not in rec:
                            continue
                        bars += 1
                        mismatches += 0 if rec["match"] else 1
                        entries += 0 if rec.get("live_open_action") in (None, "none") else 1
                        if "batch_signal" in rec and (rec["solo_signal"] != rec["batch_signal"]
                                                      or rec["solo_open_action"] != rec["batch_open_action"]):
                            batch += 1
                os.remove(jsonl)
            rows.append({
                "invocation": "uv run --no-sync python backtest/parity_diff.py " + " ".join(
                    a if " " not in a and "{" not in a else "'" + a + "'" for a in base + list(extra)),
                "exit_code": proc.returncode,
                "bars_compared": bars,
                "mismatches": mismatches,
                "batch_mismatches": batch if "--batched" in extra else None,
                "entry_decisions": entries,
                "stdout_tail": [l for l in proc.stdout.splitlines() if l.strip()][-6:],
                "stderr_tail": ([l for l in proc.stderr.splitlines() if l.strip()][-3:]
                                if proc.returncode not in (0, 3) else []),
            })
    return {"input_rows": int(len(frame)), "input_first": str(frame.index[0]), "input_last": str(frame.index[-1]),
            "input_sha256": frame_sha256(frame), "runs": rows}


def _score_proxy(ns, reg, arm: dict, wname: str, memo: dict, funding_mode: str, contract: dict) -> dict:
    window = ns.windows[wname]
    key = (wname, funding_mode, ns.ew.incumbent_contract_identity(contract))
    if key not in memo:
        memo[key] = ns.ew.incumbent_bars(ns.ew.compute_incumbent_legs(
            reg, ns.datasets, window, CAPITAL, funding_mode=funding_mode, contract=contract))
    ns.ew.validate_candidate(arm)
    legs = {}
    for symbol, timeframe in ns.datasets:
        legs[ns.ew.dataset_key(symbol, timeframe)] = ns.ew.run_candidate_leg(
            reg, arm, symbol, timeframe, window, CAPITAL, keep_trades=True, funding_mode=funding_mode)
    score = ns.ew.score_candidate(legs, memo[key])
    score["bars"] = memo[key]
    score["close_validation"] = ns.aggregate_close_validations(
        [leg.get("close_validation") for leg in legs.values() if leg is not None])
    return summarize(score, venue=False)


def run(cache_db: str, skip_cli_parity: bool = False) -> dict:
    import eval_windows as ew
    import offline_manifest as om
    from backtester import aggregate_close_validations
    from registry_loader import load_registry

    seed = load_seed()
    proto = protocol(seed)
    freeze = _load_json(PROXY_FREEZE)
    verified = verify_proxy_freeze(freeze)
    block_network()
    reg = load_registry("futures")
    defaults = dict(reg.STRATEGY_REGISTRY[CANDIDATE]["default_params"])
    if defaults != seed["params"]:
        raise SystemExit("candidate seed params must equal the registry defaults")
    contract = contract_of(seed)
    ns = SimpleNamespace(ew=ew, windows=proxy_windows(), datasets=proxy_datasets(),
                         aggregate_close_validations=aggregate_close_validations)
    comparator_params = {n: dict(reg.STRATEGY_REGISTRY[n]["default_params"]) for n in COMPARATORS}

    memo: dict = {}
    proxy_train = []
    for label, params in _grid(defaults):
        s = _score_proxy(ns, reg, arm_spec(seed, CANDIDATE, params), PROXY_SELECTION_WINDOW, memo, "charge", contract)
        proxy_train.append({"label": label, "params": params,
                            "summary": _brief(s, drop=("per_dataset", "positions_list", "close_validation"))})
    proxy_sel = select(proxy_train, len(ns.datasets), venue=False)
    proxy_selected = proxy_sel["params"] if proxy_sel["selected"] else defaults
    proxy_arms = {"candidate": arm_spec(seed, CANDIDATE, proxy_selected)}
    proxy_arms.update({n: arm_spec(seed, n, comparator_params[n]) for n in COMPARATORS})

    proxy_full = {}
    proxy_off = {}
    for wname in PROXY_HELD_OUT_WINDOWS:
        proxy_full[wname] = {k: _score_proxy(ns, reg, a, wname, memo, "charge", contract) for k, a in proxy_arms.items()}
        proxy_off[wname] = {k: _brief(_score_proxy(ns, reg, a, wname, memo, "off", contract))
                            for k, a in proxy_arms.items()}
    proxy_held = {w: {k: _brief(s) for k, s in arms.items()} for w, arms in proxy_full.items()}
    for w, arms in proxy_full.items():
        proxy_held[w]["candidate"]["per_dataset"] = arms["candidate"]["per_dataset"]
    proxy_defaults_ref = _brief(_score_proxy(ns, reg, arm_spec(seed, CANDIDATE, defaults), PROXY_PRIMARY_WINDOW,
                                             memo, "charge", contract))
    proxy_variants = []
    for row in proxy_train:
        held = _score_proxy(ns, reg, arm_spec(seed, CANDIDATE, row["params"]), PROXY_PRIMARY_WINDOW, memo,
                            "charge", contract)
        proxy_variants.append({"label": row["label"], "train_mean_sharpe": row["summary"]["mean_sharpe"],
                               "train_positions": row["summary"]["positions"],
                               "train_eligible": eligible(row["summary"], len(ns.datasets), False),
                               "test_mean_sharpe": held["mean_sharpe"],
                               "test_mean_return_pct": held["mean_return_pct"],
                               "test_positions": held["positions"], "test_clusters": held["clusters"],
                               "test_m1_verdict": held["incumbent_m1_verdict"]})
    proxy_out = proxy_verdict(proxy_held, proxy_sel)
    proxy_frames, proxy_tf, proxy_coin = {}, {}, {}
    for symbol, timeframe in ns.datasets:
        key = ew.dataset_key(symbol, timeframe)
        proxy_frames[key] = _proxy_frame(symbol, timeframe, ns.windows[PROXY_PRIMARY_WINDOW])
    proxy_audit = entry_audit(reg, proxy_frames, proxy_selected,
                              proxy_full[PROXY_PRIMARY_WINDOW]["candidate"]["positions_list"])

    manifest = om.load_manifest(MANIFEST)
    vds = ew.manifest_datasets(manifest)
    venue_memo: dict = {}

    def vscore(name, params, window, cost):
        s = ew.evaluate_window(reg, arm_spec(seed, name, params), vds, window, CAPITAL, venue_memo,
                               manifest=manifest, cost_multiplier=cost, incumbent_contract=contract)
        return summarize(s, venue=True)

    venue_train = []
    for label, params in _grid(defaults):
        s = vscore(CANDIDATE, params, VENUE_SELECTION_WINDOW, BASE_COST)
        venue_train.append({"label": label, "params": params,
                            "summary": _brief(s, drop=("per_dataset", "positions_list", "close_validation"))})
    venue_sel = select(venue_train, len(vds), venue=True)
    venue_selected = venue_sel["params"] if venue_sel["selected"] else defaults
    venue_arms = {"candidate": (CANDIDATE, venue_selected)}
    venue_arms.update({n: (n, comparator_params[n]) for n in COMPARATORS})
    venue_full = {k: {"base": vscore(n, p, VENUE_HELD_OUT_WINDOW, BASE_COST),
                      "stress": vscore(n, p, VENUE_HELD_OUT_WINDOW, STRESS_COST)} for k, (n, p) in venue_arms.items()}
    venue_held = {k: {c: _brief(s) for c, s in v.items()} for k, v in venue_full.items()}
    venue_held["candidate"]["base"]["per_dataset"] = venue_full["candidate"]["base"]["per_dataset"]
    venue_variants = []
    for row in venue_train:
        held = vscore(CANDIDATE, row["params"], VENUE_HELD_OUT_WINDOW, BASE_COST)
        venue_variants.append({"label": row["label"], "train_mean_sharpe": row["summary"]["mean_sharpe"],
                               "train_positions": row["summary"]["positions"],
                               "train_eligible": eligible(row["summary"], len(vds), True),
                               "test_mean_sharpe": held["mean_sharpe"],
                               "test_mean_return_pct": held["mean_return_pct"],
                               "test_positions": held["positions"], "test_clusters": held["clusters"],
                               "test_m1_verdict": held["incumbent_m1_verdict"]})
    venue_out = venue_verdict(venue_held, venue_sel, manifest["provenance"]["kind"])
    venue_frames, venue_tf, venue_coin = {}, {}, {}
    for ds in manifest["datasets"]:
        frame, _, _ = om.window_frame(manifest, ds, VENUE_HELD_OUT_WINDOW)
        venue_frames[ds["key"]] = frame
        venue_tf[ds["key"]] = manifest["interval"]
        venue_coin[ds["key"]] = ds["coin"]
    venue_audit = entry_audit(reg, venue_frames, venue_selected, venue_full["candidate"]["base"]["positions_list"])

    overall = overall_verdict(proxy_out, venue_out)
    costs = manifest["costs"]
    venue_gate = costs["min_notional_usd"] * (1.0 + costs["min_notional_margin"])
    proxy_min = min((s["min_entry_notional_usd"] for arms in proxy_held.values() for s in arms.values()
                     if s.get("min_entry_notional_usd") is not None), default=None)
    return {
        "study": manifest["study"],
        "issue": ISSUE,
        "tracking_group": TRACKING_GROUP,
        "protocol": proto,
        "protocol_sha256": protocol_sha256(proto),
        "proxy_freeze": {"path": os.path.relpath(PROXY_FREEZE, _REPO), "sha256": _sha_file(PROXY_FREEZE),
                         "verification": verified, "network_blocked_while_scoring": True},
        "manifest": {
            "path": os.path.relpath(manifest["path"], _REPO),
            "sha256": om.sha256_file(manifest["path"]),
            "provenance": manifest["provenance"],
            "interval": manifest["interval"],
            "warmup_bars": manifest["warmup_bars"],
            "windows": {k: {"start": v["start"], "end": v["end"], "role": v["role"]}
                        for k, v in manifest["windows"].items()},
            "costs": costs,
            "datasets": [{"key": d["key"], "size_decimals": d["size_decimals"],
                          "half_spread_bps": d["half_spread_bps"],
                          "candles_sha256": d["candles"]["sha256"],
                          "funding_sha256": d["funding"]["sha256"] if d["funding"] else None}
                         for d in manifest["datasets"]],
        },
        "source_sha256": {p: _sha_file(os.path.join(_REPO, p)) for p in SOURCE_FILES},
        "comparator_params": comparator_params,
        "incumbents_reference": list(ew.INCUMBENTS),
        "proxy": {
            "train_sweep": proxy_train,
            "selection": proxy_sel,
            "selected_params": proxy_selected,
            "held_out": proxy_held,
            "funding_off_sensitivity": proxy_off,
            "defaults_oos": proxy_defaults_ref,
            "variants_oos": proxy_variants,
            "stability": {
                "spearman_train_vs_oos_sharpe": spearman([v["train_mean_sharpe"] for v in proxy_variants],
                                                         [v["test_mean_sharpe"] for v in proxy_variants]),
                "oos_net_positive_variants": sum(1 for v in proxy_variants if v["test_mean_return_pct"] > 0),
                "variants": len(proxy_variants),
                "selected_plateau": neighbors(proxy_selected, proxy_train) if proxy_sel["selected"] else None,
            },
            "entry_audit": entry_audit_brief(proxy_audit),
            "smallest_order": {"min_entry_notional_usd": proxy_min, "venue_gate_usd": round(venue_gate, 4),
                               "below_gate": (proxy_min is not None and proxy_min < venue_gate)},
            "limitations": ("Binance US USDT candles stand in for Hyperliquid prices. Proxy legs use the flat "
                            "M1 cost model (Hyperliquid taker fee plus the engine's default slippage); they model "
                            "no lot rounding, no minimum order and no spread. The smallest simulated proxy order "
                            "is compared with the pinned venue gate above."),
            "verdict": proxy_out,
        },
        "venue": {
            "train_sweep": venue_train,
            "selection": venue_sel,
            "selected_params": venue_selected,
            "held_out": venue_held,
            "variants_test": venue_variants,
            "stability": {
                "spearman_train_vs_test_sharpe": spearman([v["train_mean_sharpe"] for v in venue_variants],
                                                          [v["test_mean_sharpe"] for v in venue_variants]),
                "test_net_positive_variants": sum(1 for v in venue_variants if v["test_mean_return_pct"] > 0),
                "variants": len(venue_variants),
                "selected_plateau": neighbors(venue_selected, venue_train) if venue_sel["selected"] else None,
            },
            "entry_audit": entry_audit_brief(venue_audit),
            "frozen_parity": frozen_parity(venue_frames, seed, venue_selected, venue_tf, venue_coin),
            "limitations": ("The venue windows cover 2024-09-01 to 2026-09-01 only (the venue serves the most "
                            "recent 5,000 candles); the M1 2023 and 2024 windows have no venue equivalent. "
                            "Spread is a per-coin half spread from one frozen L2 snapshot plus flat slippage; "
                            "queue position, partial fills, liquidation and funding-rate impact are not modelled."),
            "verdict": venue_out,
        },
        "cli_parity": None if skip_cli_parity else cli_parity(seed, proxy_selected, cache_db),
        "verdict": overall,
        "verdict_guards": verdict_guards(proxy_held, proxy_sel, venue_held, venue_sel),
    }


def entry_audit_brief(rows: list) -> list:
    return rows


def _fmt(v, nd=2):
    if v is None:
        return "-"
    if isinstance(v, bool):
        return "yes" if v else "no"
    if isinstance(v, float):
        return f"{v:.{nd}f}"
    return str(v)


ARM_LABELS = {"candidate": "aroon_trend (selected)", "adx_trend": "adx_trend defaults", "breakout": "breakout defaults"}
ARM_HEADER = ("| Arm | Mean ret % | Mean Sharpe | Mean DDadj | Worst DD % | Net $ | Fees $ | Funding $ | Turnover | "
              "Positions (L/S) | Clusters | Unpriced held h | M1 verdict |")
ARM_RULE = "|---|---|---|---|---|---|---|---|---|---|---|---|---|"


def _arm_row(label: str, s: dict) -> str:
    return (f"| {label} | {_fmt(s.get('mean_return_pct'))} | {_fmt(s.get('mean_sharpe'), 3)} | "
            f"{_fmt(s.get('mean_ddadj'), 3)} | {_fmt(s.get('worst_max_dd_pct'))} | {_fmt(s.get('net_pnl_usd'))} | "
            f"{_fmt(s.get('fees_usd'))} | {_fmt(s.get('funding_pnl_usd'))} | {_fmt(s.get('mean_turnover'))} | "
            f"{s.get('positions')} ({s.get('long_positions')}/{s.get('short_positions')}) | {s.get('clusters')} | "
            f"{s.get('funding_unpriced_held_hours')} | {s.get('incumbent_m1_verdict')} |")


def _variant_table(variants: list) -> list:
    lines = ["| Variant | Train Sharpe | Train eligible | Held-out Sharpe | Held-out ret % | Positions | Clusters | M1 |",
             "|---|---|---|---|---|---|---|---|"]
    for row in sorted(variants, key=lambda r: (-r["test_mean_sharpe"], r["label"])):
        lines.append(f"| {row['label']} | {_fmt(row['train_mean_sharpe'], 3)} | {_fmt(row['train_eligible'])} | "
                     f"{_fmt(row['test_mean_sharpe'], 3)} | {_fmt(row['test_mean_return_pct'])} | "
                     f"{row['test_positions']} | {row['test_clusters']} | {row['test_m1_verdict']} |")
    return lines


def _audit_table(rows: list) -> list:
    lines = ["| Dataset | Side | Signal bar | Fill bar | Exit | Net $ | Prev Up/Down | Up/Down | Ages H/L | Input sha256 |",
             "|---|---|---|---|---|---|---|---|---|---|"]
    for e in rows:
        lines.append(f"| {e['dataset']} | {e['side']} | {e['signal_bar']} | {e['fill_bar']} | "
                     f"{','.join(e['exit_reasons'] or [])} | {_fmt(e['net_pnl_usd'])} | "
                     f"{_fmt(e['prev_up'], 1)}/{_fmt(e['prev_down'], 1)} | {_fmt(e['up'], 1)}/{_fmt(e['down'], 1)} | "
                     f"{e['high_age']}/{e['low_age']} | `{e['input_sha256'][:16]}` |")
    return lines


def _funding_table(per_dataset: dict) -> list:
    lines = ["| Dataset | Ret % | Sharpe | Positions (L/S) | Funding mode | Source | Expected h | Present h | Missing h | "
             "Venue-absent h | Unpriced held h | Funding sha256 |",
             "|---|---|---|---|---|---|---|---|---|---|---|---|"]
    for ds, s in sorted(per_dataset.items()):
        f = s["funding"]
        lines.append(f"| {ds} | {_fmt(s['return_pct'])} | {_fmt(s['sharpe'], 3)} | "
                     f"{s['positions']} ({s['long_positions']}/{s['short_positions']}) | {f['mode']} | {f['source']} | "
                     f"{f['expected_hours']} | {f['present_hours']} | {f['missing_hours']} | {f['venue_absent_hours']} | "
                     f"{f['unpriced_held_hours']} | `{(f['sha256'] or '')[:16]}` |")
    return lines


def render(result: dict) -> str:
    v = result["verdict"]
    p, ve = result["proxy"], result["venue"]
    proto = result["protocol"]
    seed = proto["candidate_seed"]
    tier = seed["close_strategies"][0]["params"]["tp_tiers"][0]
    m = result["manifest"]
    lines = [
        f"# Aroon trend: frozen Hyperliquid study (#{result['issue']})",
        "",
        f"Overall verdict: **{v['outcome'].upper()}** (proxy {v['proxy']}, venue {v['venue']}).",
        "",
        f"- Proxy M1 conclusion: **{p['verdict']['outcome']}**. {p['verdict']['reason']}.",
        f"- Venue-input conclusion: **{ve['verdict']['outcome']}**. {ve['verdict']['reason']}.",
        "",
        f"Registry evidence source for this outcome: `{v['edge_source']}`. {v['availability']}.",
        "",
        "Generated by `run_study.py` from `results.json`; do not edit by hand. Proxy candles are not evidence "
        "of a Hyperliquid edge.",
        "",
        "## Frozen inputs",
        "",
        f"- Protocol sha256 `{result['protocol_sha256']}` (printed by `--protocol-hash` before scoring)",
        f"- Proxy freeze `{result['proxy_freeze']['path']}` sha256 `{result['proxy_freeze']['sha256']}`; "
        f"{result['proxy_freeze']['verification']['verified_frames']} frame digests and funding digests for "
        f"{', '.join(result['proxy_freeze']['verification']['verified_funding'])} verified before scoring; "
        "network fetches were blocked while scoring.",
        f"- Venue manifest `{m['path']}` sha256 `{m['sha256']}` ({m['provenance']['kind']}: {m['provenance']['label']})",
        f"- Contract for every arm and every M1 incumbent: direction `both`, one `tiered_tp_pct` close (profit "
        f"{tier['profit_pct']}, close fraction {tier['close_fraction']}), one fixed entry ATR x{seed['stop_loss_atr_mult']} "
        f"stop, comparison mode `{seed['comparison_mode']}`, capital ${proto['capital']:.0f}, intrabar "
        f"`{proto['intrabar_resolution']}`. Entries decided on bar N fill at the bar N+1 open.",
        f"- Comparators: {', '.join(f'`{k}` {json.dumps(val, sort_keys=True)}' for k, val in sorted(result['comparator_params'].items()))}",
        f"- Selection rule: {proto['selection_rule']}.",
        f"- Proxy verdict rule: {proto['proxy']['verdict_rule']}.",
        f"- Venue verdict rule: {proto['venue']['verdict_rule']}.",
        f"- Overall rule: {proto['overall_rule']}.",
        "",
        "## Proxy M1 study (Binance US candles, Hyperliquid fees and funding)",
        "",
    ]
    for name, (start, end) in sorted(proto["proxy"]["windows"].items(), key=lambda kv: kv[1][0]):
        lines.append(f"- Window `{name}` [{start}, {end})")
    sel = p["selection"]
    lines += [
        "",
        f"Selection on `is`: rule outcome `{sel['rule_outcome']}`, {sel.get('eligible', 0)} of "
        f"{len(p['train_sweep'])} combinations eligible; selected `{json.dumps(p['selected_params'], sort_keys=True)}`.",
        "",
    ]
    for wname in PROXY_HELD_OUT_WINDOWS:
        lines += [f"### Window `{wname}` (funding charge)", "", ARM_HEADER, ARM_RULE]
        for k, label in ARM_LABELS.items():
            lines.append(_arm_row(label, p["held_out"][wname][k]))
        lines.append("")
    lines += ["### Candidate funding coverage per dataset (oos, charge)", ""]
    lines += _funding_table(p["held_out"]["oos"]["candidate"]["per_dataset"])
    lines += ["", "### Funding-off sensitivity (labelled; never a promotion input)", "", ARM_HEADER, ARM_RULE]
    for wname in PROXY_HELD_OUT_WINDOWS:
        for k, label in ARM_LABELS.items():
            lines.append(_arm_row(f"{wname}: {label}", p["funding_off_sensitivity"][wname][k]))
    so = p["smallest_order"]
    st = p["stability"]
    d = p["defaults_oos"]
    lines += [
        "",
        f"- Proxy limitations: {p['limitations']} Smallest simulated proxy order ${_fmt(so['min_entry_notional_usd'])} "
        f"against the venue gate ${_fmt(so['venue_gate_usd'])} (below gate: {_fmt(so['below_gate'])}).",
        f"- Registry defaults in oos (reference only): mean ret {_fmt(d.get('mean_return_pct'))}%, Sharpe "
        f"{_fmt(d.get('mean_sharpe'), 3)}, positions {d.get('positions')}, M1 {d.get('incumbent_m1_verdict')}.",
        f"- Parameter stability: Spearman train vs oos Sharpe {_fmt(st['spearman_train_vs_oos_sharpe'], 3)}; "
        f"oos net-positive variants {st['oos_net_positive_variants']}/{st['variants']}; plateau "
        f"{json.dumps(st['selected_plateau'], sort_keys=True)}.",
        "",
        "Proxy checks: " + ", ".join(f"{k} {_fmt(val)}" for k, val in sorted(p["verdict"]["checks"].items())) +
        f"; secondary M1 passes {p['verdict']['secondary_m1_passes']}.",
        "",
        "### Proxy variants in oos (every combination; non-positive rows are unsuccessful)",
        "",
    ]
    lines += _variant_table(p["variants_oos"])
    lines += ["", "### Proxy entry audit (oos, selected parameters)", ""]
    lines += _audit_table(p["entry_audit"])
    c = m["costs"]
    vsel = ve["selection"]
    lines += [
        "",
        "## Venue-input study (frozen Hyperliquid candles and funding, execution specification)",
        "",
        f"- Costs: taker {c['taker_fee_pct']*100:.3f}%, maker {c['maker_fee_pct']*100:.3f}%, slippage "
        f"{c['slippage_bps']} bps per side plus per-coin half spread, lot rounding to szDecimals, minimum order "
        f"${c['min_notional_usd']:.0f} with a {c['min_notional_margin']*100:.0f}% margin, funding booked hourly. "
        "Identical for candidate, comparators and M1 incumbents.",
    ]
    for name, w in sorted(m["windows"].items(), key=lambda kv: kv[1]["start"]):
        lines.append(f"- Window `{name}` [{w['start']}, {w['end']}): {w['role']}")
    for dsi in m["datasets"]:
        lines.append(f"- `{dsi['key']}`: szDecimals {dsi['size_decimals']}, half spread {dsi['half_spread_bps']} bps, "
                     f"candles `{dsi['candles_sha256'][:16]}`, funding `{(dsi['funding_sha256'] or '')[:16]}`")
    lines += [
        f"- Limitations: {ve['limitations']}",
        "",
        f"Selection on `train`: rule outcome `{vsel['rule_outcome']}`, {vsel.get('eligible', 0)} of "
        f"{len(ve['train_sweep'])} combinations eligible; selected `{json.dumps(ve['selected_params'], sort_keys=True)}`.",
        "",
        "### Held-out `test`",
        "",
        ARM_HEADER.replace("| Arm |", "| Arm | Cost |"),
        ARM_RULE + "---|",
    ]
    for k, label in ARM_LABELS.items():
        for cost in ("base", "stress"):
            row = _arm_row(label, ve["held_out"][k][cost])
            lines.append(row.replace(f"| {label} |", f"| {label} | {'x1' if cost == 'base' else 'x2'} |", 1))
    vst = ve["stability"]
    lines += [
        "",
        f"- Parameter stability: Spearman train vs test Sharpe {_fmt(vst['spearman_train_vs_test_sharpe'], 3)}; "
        f"test net-positive variants {vst['test_net_positive_variants']}/{vst['variants']}; plateau "
        f"{json.dumps(vst['selected_plateau'], sort_keys=True)}.",
        "",
        "Venue checks: " + ", ".join(f"{k} {_fmt(val)}" for k, val in sorted(ve["verdict"]["checks"].items())) + ".",
        "",
        "### Candidate per dataset (test, x1)",
        "",
    ]
    lines += _funding_table(ve["held_out"]["candidate"]["base"]["per_dataset"])
    lines += ["", "### Venue variants in test", ""]
    lines += _variant_table(ve["variants_test"])
    lines += ["", "### Venue entry audit (test, selected parameters)", ""]
    lines += _audit_table(ve["entry_audit"])
    lines += ["", "## Decision parity", "",
              "Frozen venue frames (held-out window with warm-up), selected venue parameters:", "",
              "| Dataset | Window | Batched | Input sha256 | Compared bars | Entry decisions | Mismatches | Batch diffs | Close parity |",
              "|---|---|---|---|---|---|---|---|---|"]
    for row in ve["frozen_parity"]:
        lines.append(f"| {row['dataset']} | {row['window']} | {_fmt(row['batched'])} | `{row['input_sha256'][:16]}` | "
                     f"{row['compared_bars']} | {row['entry_decisions']} | {row['mismatches']} | "
                     f"{_fmt(row['batch_mismatches'])} | {row.get('close_parity') or '-'} |")
    cp = result["cli_parity"] or {"runs": [], "input_rows": None, "input_sha256": "", "input_first": None,
                                  "input_last": None}
    lines += ["", f"`parity_diff.py` CLI on the isolated proxy cache with network fetches blocked "
              f"({CLI_PARITY_SYMBOL} {CLI_PARITY_TIMEFRAME}, {cp['input_rows']} rows {cp['input_first']} to "
              f"{cp['input_last']}, input sha256 `{(cp['input_sha256'] or '')[:16]}`), selected proxy parameters, "
              "the study close and strict comparison mode. Exit 0 is clean; 3 is decision agreement without "
              "close-parity proof:", "",
              "| Invocation | Exit | Compared bars | Entry decisions | Mismatches | Batch diffs |", "|---|---|---|---|---|---|"]
    for row in cp["runs"]:
        lines.append(f"| `{row['invocation']}` | {row['exit_code']} | {row['bars_compared']} | "
                     f"{row['entry_decisions']} | {row['mismatches']} | {_fmt(row['batch_mismatches'])} |")
    g = result["verdict_guards"]
    lines += ["", "## Verdict guards", "",
              "The same rules on altered copies of this result: " + ", ".join(f"{k} -> {g[k]}" for k in sorted(g)) + ".",
              "", "## Source hashes", ""]
    for path, digest in sorted(result["source_sha256"].items()):
        lines.append(f"- `{path}` `{digest}`")
    lines.append("")
    return "\n".join(lines)


def main(argv=None) -> int:
    p = argparse.ArgumentParser(description="#1643 frozen Aroon trend study driver (research only, suggest-only)")
    p.add_argument("--cache-db", default=None, help="Isolated SQLite cache for the proxy candles and funding")
    p.add_argument("--freeze-proxy", action="store_true",
                   help="Fetch the proxy inputs into --cache-db and write proxy_freeze.json (before any scoring)")
    p.add_argument("--json", default=os.path.join(_HERE, "results.json"), dest="json_out")
    p.add_argument("--report", default=os.path.join(_HERE, "REPORT.md"))
    p.add_argument("--render-only", action="store_true", help="Re-render the report from the saved results JSON")
    p.add_argument("--protocol-hash", action="store_true", help="Print the frozen protocol sha256 and exit")
    p.add_argument("--skip-cli-parity", action="store_true", help="Skip the parity_diff.py subprocess runs")
    args = p.parse_args(argv)
    if args.render_only:
        result = _load_json(args.json_out)
        with open(args.report, "w") as fh:
            fh.write(render(result))
        print(f"wrote {args.report}")
        return 0
    if args.protocol_hash:
        print(protocol_sha256(protocol(load_seed())))
        return 0
    if not args.cache_db:
        raise SystemExit("--cache-db is required (an isolated proxy cache, never the shared one)")
    cache_db = os.path.abspath(args.cache_db)
    os.environ[CACHE_ENV] = cache_db
    if args.freeze_proxy:
        freeze = freeze_proxy(cache_db)
        with open(PROXY_FREEZE, "w") as fh:
            json.dump(freeze, fh, indent=1, sort_keys=True)
            fh.write("\n")
        print(f"wrote {PROXY_FREEZE}")
        return 0
    result = run(cache_db, skip_cli_parity=args.skip_cli_parity)
    with open(args.json_out, "w") as fh:
        json.dump(result, fh, indent=1, sort_keys=True, default=str)
        fh.write("\n")
    for name, spec in (("held_out_candidate_proxy.json", arm_spec(result["protocol"]["candidate_seed"], CANDIDATE,
                                                                  result["proxy"]["selected_params"])),
                       ("held_out_candidate_venue.json", arm_spec(result["protocol"]["candidate_seed"], CANDIDATE,
                                                                  result["venue"]["selected_params"]))):
        with open(os.path.join(os.path.dirname(os.path.abspath(args.json_out)), name), "w") as fh:
            json.dump(spec, fh, indent=2)
            fh.write("\n")
    with open(args.report, "w") as fh:
        fh.write(render(result))
    print(f"verdict: {result['verdict']['outcome']} (proxy {result['verdict']['proxy']}, venue {result['verdict']['venue']})")
    print(f"registry edge_source for this outcome: {result['verdict']['edge_source']}")
    print(f"wrote {args.json_out} and {args.report}")
    return 0


if __name__ == "__main__":
    sys.exit(main())

#!/usr/bin/env python3

import argparse
import hashlib
import importlib.util
import itertools
import json
import os
import sys

import numpy as np
import pandas as pd

_HERE = os.path.dirname(os.path.abspath(__file__))
_BACKTEST = os.path.abspath(os.path.join(_HERE, "..", ".."))
_REPO = os.path.abspath(os.path.join(_BACKTEST, ".."))
for _p in (os.path.join(_REPO, "shared_tools"), _BACKTEST):
    if _p not in sys.path:
        sys.path.insert(0, _p)

import offline_manifest as om
import trade_flow_replay as tfr
from atr import ensure_atr_indicator
from backtester import Backtester
from registry_loader import load_registry


def _load_local(name, filename):
    spec = importlib.util.spec_from_file_location(name, os.path.join(_HERE, filename))
    mod = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(mod)
    return mod


arms = _load_local("_sai_study_arms", "arms.py")
_SAI = _load_local("_sai_study_module", os.path.join("..", "..", "..", "shared_strategies", "open",
                                                     "sweep_absorption_ifvg.py"))

STUDY_MANIFEST = os.path.join(_HERE, "study_manifest.json")
RESULTS = os.path.join(_HERE, "results.json")
REPORT = os.path.join(_HERE, "REPORT.md")
CANDIDATE = "sweep_absorption_ifvg"
CONTROLS = ("amd_ifvg", "liquidity_sweeps")
CAPITAL = 10_000.0
CONTROL_WINDOW = 200
SOURCE_FILES = (
    "shared_strategies/open/sweep_absorption_ifvg.py",
    "shared_strategies/open/cumulative_volume_delta_divergence.py",
    "shared_strategies/open/registry.py",
    "shared_scripts/check_hyperliquid.py",
    "shared_tools/market_payload.py",
    "backtest/backtester.py",
    "backtest/offline_manifest.py",
    "backtest/trade_flow_replay.py",
    "backtest/candidates/sweep_absorption_ifvg_2010/run_study.py",
    "backtest/candidates/sweep_absorption_ifvg_2010/arms.py",
    "backtest/candidates/sweep_absorption_ifvg_2010/candidate_spec.json",
    "scheduler/sweep_absorption_ifvg.go",
    "scheduler/market_feed_trades.go",
    "scheduler/market_feed_requirements.go",
)


def _sha(path):
    return om.sha256_file(path)


def _canon(value):
    return json.dumps(value, sort_keys=True, separators=(",", ":"))


def _rel(base, path):
    return os.path.abspath(os.path.join(base, path))


def load_study(path=STUDY_MANIFEST):
    with open(path) as fh:
        study = json.load(fh)
    base = os.path.dirname(os.path.abspath(path))
    spec_path = _rel(base, study["spec"]["path"])
    if _sha(spec_path) != study["spec"]["sha256"]:
        raise SystemExit(f"{spec_path} no longer matches its frozen sha256; a changed specification is a new study")
    with open(spec_path) as fh:
        spec = json.load(fh)
    return study, spec, base


def _file_gate(base, ref, gate):
    path = _rel(base, ref["path"])
    if not os.path.exists(path):
        return {"gate": gate, "path": ref["path"], "reason": "not acquired: the file does not exist"}
    if not ref.get("sha256"):
        return {"gate": gate, "path": ref["path"], "reason": "no pinned sha256"}
    if os.path.isfile(path) and _sha(path) != ref["sha256"]:
        return {"gate": gate, "path": ref["path"], "reason": "sha256 does not match the pinned value"}
    return None


def preflight(study, spec, base):
    gates = []
    rec = study["recording"]
    run_manifest = _rel(base, os.path.join(rec["path"], "run.manifest.json"))
    recording = None
    if not os.path.exists(run_manifest):
        gates.append({"gate": "source_b_recording", "path": rec["path"],
                      "reason": "no forward trade-flow recording has been captured for this study"})
    else:
        gate = _file_gate(base, {"path": os.path.join(rec["path"], "run.manifest.json"), "sha256": rec.get("sha256")},
                          "source_b_recording")
        if gate:
            gates.append(gate)
        else:
            try:
                recording = tfr.load_trade_recording(_rel(base, rec["path"]))
            except (tfr.TradeRecordingError, OSError, ValueError) as exc:
                gates.append({"gate": "source_b_recording", "path": rec["path"], "reason": f"refused: {exc}"})
            else:
                if not recording["report"].get("economic_evidence"):
                    gates.append({"gate": "source_b_recording", "path": rec["path"],
                                  "reason": "the recording is not economic evidence: "
                                            + "; ".join(recording["report"].get("refusals") or [])})
    for ref, gate in ((study["side_evidence"], "side_evidence"), (study["sealed_payloads"], "sealed_replay_proof")):
        g = _file_gate(base, ref, gate)
        if g:
            gates.append(g)
    manifests = {}
    for interval in spec["datasets"]["intervals"]:
        ref = study["offline_manifests"][interval]
        g = _file_gate(base, ref, f"offline_manifest_{interval}")
        if g:
            gates.append(g)
            for coin in spec["datasets"]["coins"]:
                gates.append({"gate": "dataset", "dataset": f"{coin} {interval}",
                              "reason": f"needs candles, funding, trade_flow and htf_candles in {ref['path']}"})
            continue
        try:
            manifest = om.load_manifest(_rel(base, ref["path"]), verify=True)
        except (om.ManifestError, OSError, ValueError) as exc:
            gates.append({"gate": f"offline_manifest_{interval}", "path": ref["path"], "reason": f"refused: {exc}"})
            continue
        problems = []
        if manifest["schema"] != om.SCHEMA_V7 or manifest["interval"] != interval:
            problems.append(f"needs schema {om.SCHEMA_V7} at interval {interval}")
        for name, win in spec["windows"].items():
            got = manifest["windows"].get(name)
            if not got or got["start"] != win["start"] or got["end"] != win["end"]:
                problems.append(f"window {name} differs from the frozen specification")
        need = max_support_bars(spec, interval)
        if manifest["warmup_bars"] < need:
            problems.append(f"warmup_bars {manifest['warmup_bars']} is below the {need}-bar support horizon")
        for msg in problems:
            gates.append({"gate": f"offline_manifest_{interval}", "path": ref["path"], "reason": msg})
        for coin in spec["datasets"]["coins"]:
            key = f"{coin} {interval}"
            ds = next((d for d in manifest["datasets"] if d["key"] == key), None)
            missing = [k for k in ("funding", "trade_flow", "htf_candles") if ds is None or ds.get(k) is None]
            if ds is None or missing:
                gates.append({"gate": "dataset", "dataset": key,
                              "reason": "missing " + ", ".join(["dataset"] if ds is None else missing)})
        if not problems:
            manifests[interval] = manifest
    return gates, manifests, recording


def _sealed_frame(market, coin, tf, cutoff, keep):
    from closed_bar import FRAME_DECISION_TIME_ATTR, RULE_HYPERLIQUID_NATIVE, sealed_frame_timings, select_closed
    from market_payload import market_frame_rows_with_timing
    rows, timing = market_frame_rows_with_timing(market, coin, tf)
    sel = select_closed(rows, sealed_frame_timings(rows, timing, tf), timeframe=tf, cutoff_ms=cutoff,
                        rule=RULE_HYPERLIQUID_NATIVE, keep=keep, min_rows=1)
    opens = [int(t.open_ms) for t in sel.timings]
    frame = pd.DataFrame([r[1:6] for r in sel.rows], columns=["open", "high", "low", "close", "volume"],
                         index=pd.to_datetime(opens, unit="ms"))
    frame.attrs[FRAME_DECISION_TIME_ATTR] = {"cutoff_ms": int(cutoff), "boundary_ms": int(sel.boundary_ms)}
    return frame, [[o] + [float(x) for x in r[1:6]] for o, r in zip(opens, sel.rows)], sel.boundary_ms


def sealed_replay_proof(base, study, manifests):
    from market_payload import market_decision_cutoff_ms, market_trade_flow
    ref = study["sealed_payloads"]
    with open(_rel(base, ref["path"])) as fh:
        index = json.load(fh)
    reg = load_registry("futures")
    compared, mismatches = 0, []
    cols = ["signal", "sai_reason", "sai_setup_id", "sai_flow_sha256", "sai_entry_price", "sai_absorption_ratio"]
    for item in index.get("payloads") or []:
        g = _file_gate(base, item, "sealed_replay_proof")
        if g:
            mismatches.append(g["reason"] + f" ({item.get('path')})")
            continue
        with open(_rel(base, item["path"])) as fh:
            market = json.load(fh)["market"]
        coin, interval = item["coin"], item["interval"]
        manifest = manifests.get(interval)
        if manifest is None:
            mismatches.append(f"{item['path']}: no pinned {interval} manifest to replay against")
            continue
        series = om.load_trade_flow(om.dataset_by_key(manifest, f"{coin} {interval}"))
        cutoff = market_decision_cutoff_ms(market)
        frame, _, boundary = _sealed_frame(market, coin, interval, cutoff, 200)
        _, htf_rows, _ = _sealed_frame(market, coin, "1h", boundary, 0)
        htf = {"available": True, "kind": "htf_candles", "interval_ms": _SAI.HTF_INTERVAL_MS,
               "timestamp_kind": "open", "rows": htf_rows}
        params = dict(item.get("params") or {}, expected_interval_ms=om.INTERVAL_MS[interval], htf_candles=htf)
        sealed = reg.apply_strategy(CANDIDATE, frame, dict(params, trade_flow_observations=market_trade_flow(market, coin)))
        replayed = reg.apply_strategy(CANDIDATE, frame, dict(
            params, trade_flow_observations=tfr.payload_from_series(series, om.INTERVAL_MS[interval], cutoff)))
        compared += 1
        a = [str(x) for x in sealed[cols].iloc[-1].tolist()]
        b = [str(x) for x in replayed[cols].iloc[-1].tolist()]
        if a != b:
            mismatches.append(f"{item['path']}: sealed {a} != replayed {b}")
    if not compared:
        mismatches.append("the sealed payload index lists no comparable payload")
    return compared, mismatches


def max_support_bars(spec, interval):
    grid = spec["grid"]
    interval_ms = om.INTERVAL_MS[interval]
    keys = sorted(grid)
    out = 0
    for values in itertools.product(*(grid[k] for k in keys)):
        params = dict(zip(keys, values), expected_interval_ms=interval_ms)
        out = max(out, _SAI.support_for_params(params)["decision_bars"])
    return out


def grid_params(spec, interval):
    grid = spec["grid"]
    keys = sorted(grid)
    interval_ms = om.INTERVAL_MS[interval]
    return [dict(zip(keys, values), expected_interval_ms=interval_ms)
            for values in itertools.product(*(grid[k] for k in keys))]


def flow_usable_share(frame, flow_obs, params):
    p = {**_SAI.DEFAULT_PARAMS, **{k: v for k, v in params.items() if k in _SAI.DEFAULT_PARAMS}}
    support = _SAI.support_for_params(p)
    interval = p["expected_interval_ms"]
    flow = _SAI.parse_trade_flow(flow_obs)
    ts = frame["timestamp"].to_numpy().astype(np.int64)
    vol = frame["volume"].to_numpy(dtype="float64")
    by_ts = {int(t): i for i, t in enumerate(ts)}
    start_off = p["session_start_hour_utc"] * _SAI.HOUR_MS
    session_ms = support["session_ms"]
    usable = denom = 0
    mask = np.zeros(len(ts), dtype=bool)
    bar_ok = {}

    def ok(i, at):
        key = (i, at)
        if key in bar_ok:
            return bar_ok[key]
        total = 0.0
        good = not flow.error
        for start in range(int(ts[i]), int(ts[i]) + interval, 60_000):
            iv = flow.intervals.get(start) if good else None
            if iv is None or iv["finalized"] > at or (iv["invalid"] is not None and at >= iv["invalid"]):
                good = False
                break
            total += float(iv["total"])
        bar_ok[key] = good and total == float(vol[i])
        return bar_ok[key]

    for i, t in enumerate(ts):
        t = int(t)
        phase = (t - start_off) % _SAI.DAY_MS
        if phase < session_ms:
            continue
        j = (phase - session_ms) // interval
        if j >= support["post_bars"]:
            continue
        S = t - phase
        if S < int(ts[0]):
            continue
        denom += 1
        d = t + interval
        s_idx = by_ts.get(S)
        if s_idx is None or any(by_ts.get(int(ts[s_idx]) + k * interval) is None for k in range(i - s_idx)):
            continue
        if all(ok(k, d) for k in range(s_idx, i)):
            usable += 1
            mask[i] = True
    return {"usable": usable, "denominator": denom, "share": (usable / denom) if denom else None,
            "mask": mask}


def _window_inputs(manifest, dataset, window):
    frame, win, candle_cov = om.window_frame(manifest, dataset, window)
    frame, funding_cov = om.attach_funding_cost(frame, dataset, win)
    flow_obs, flow_cov = om.attach_trade_flow(manifest, dataset, win)
    htf_obs, htf_cov = om.attach_htf_candles(manifest, dataset, win, int(frame["timestamp"].iloc[0]))
    return frame, win, {"candles": candle_cov, "funding": funding_cov, "trade_flow": flow_cov,
                        "htf_candles": htf_cov}, flow_obs, htf_obs


def candidate_signals(reg, frame, params, flow_obs, htf_obs):
    return reg.apply_strategy(CANDIDATE, frame, {**params, "trade_flow_observations": flow_obs,
                                                 "htf_candles": htf_obs})


def causal_control_signals(reg, name, frame, eligible):
    defaults = dict(reg.get_strategy(name)["default_params"])
    out = frame.copy()
    sig = np.zeros(len(frame), dtype=np.int64)
    for i in range(len(frame)):
        if not eligible[i]:
            continue
        lo = max(0, i - CONTROL_WINDOW + 1)
        res = reg.apply_strategy(name, frame.iloc[lo:i + 1].copy(), defaults)
        sig[i] = int(res["signal"].iloc[-1])
    out["signal"] = sig
    return out, defaults


def real_backtest(manifest, dataset, win, signals, name, params, spec, cost_multiplier):
    stack = spec["execution"]["existing_owner_stack"]
    sig = om.slice_window(ensure_atr_indicator(signals), win)
    bt = Backtester(initial_capital=CAPITAL, platform="hyperliquid",
                    open_strategy={"name": name, "params": {k: v for k, v in params.items()
                                                            if not isinstance(v, dict)}},
                    close_strategies=[dict(r) for r in stack["close_strategies"]],
                    trailing_stop_atr_mult=stack["trailing_stop_atr_mult"],
                    risk_per_trade_pct=stack["risk_per_trade_pct"], direction=stack["direction"],
                    intrabar_resolution=stack["intrabar_resolution"], comparison_mode="approximate",
                    execution_spec=om.execution_spec(manifest, dataset, cost_multiplier))
    res = bt.run(sig, strategy_name=name, symbol=dataset["symbol"], timeframe=manifest["interval"], save=False)
    return res


def positions_of(trades):
    out = {}
    for t in trades or []:
        key = (str(t.get("entry_date")), str(t.get("side")))
        pos = out.setdefault(key, {"entry_date": key[0], "side": key[1], "exit_date": str(t.get("exit_date")),
                                   "notional": abs(float(t.get("entry_price") or 0) * float(t.get("shares") or 0)),
                                   "pnl": 0.0})
        pos["exit_date"] = max(pos["exit_date"], str(t.get("exit_date")))
        pos["pnl"] += float(t.get("pnl") or 0.0)
    return [out[k] for k in sorted(out)]


def independent_positions(trades):
    count, last_exit = 0, None
    for pos in positions_of(trades):
        if last_exit is None or pos["entry_date"] >= last_exit:
            count += 1
            last_exit = pos["exit_date"]
    return count


def _summary(res):
    positions = positions_of(res.get("trades"))
    wins = [p["pnl"] for p in positions if p["pnl"] > 0]
    losses = [-p["pnl"] for p in positions if p["pnl"] <= 0]
    kelly = None
    if wins and losses and sum(losses) > 0:
        rate = len(wins) / len(positions)
        kelly = rate - (1 - rate) / ((sum(wins) / len(wins)) / (sum(losses) / len(losses)))
    return {"net_return_pct": float(res.get("total_return_pct") or 0.0),
            "max_drawdown_pct": abs(float(res.get("max_drawdown_pct") or 0.0)),
            "positions": len(positions), "independent_positions": independent_positions(res.get("trades")),
            "funding_pnl": res.get("total_funding_pnl"),
            "realized_loss_usd": sum(losses), "kelly_diagnostic": kelly,
            "turnover": round(sum(p["notional"] for p in positions) / CAPITAL, 6)}


def setups_from(out, frame, htf_obs, params):
    session_ms = _SAI.support_for_params(params)["session_ms"]
    ctx = _SAI.parse_htf_candles(htf_obs)
    lows = _SAI._pivots(ctx.low, ctx.opens, lambda a, b: a < b) if not ctx.error else []
    highs = _SAI._pivots(ctx.high, ctx.opens, lambda a, b: a > b) if not ctx.error else []
    atr = ensure_atr_indicator(out.copy())["atr"].to_numpy()
    ts = frame["timestamp"].to_numpy().astype(np.int64)
    setups = []
    for i in np.flatnonzero(out["signal"].to_numpy() != 0):
        side = int(out["signal"].iloc[i])
        row = out.iloc[i]
        session = int(row["sai_session_ms"])
        decision = int(row["sai_deadline_ms"])
        mask = (ts >= session) & (ts < session + session_ms)
        opposite = float(frame["high"].to_numpy()[mask].max()) if side > 0 else float(frame["low"].to_numpy()[mask].min())
        swing = None
        piv = highs if side > 0 else lows
        usable = [p for p in piv if int(ctx.opens[p]) + (_SAI.HTF_RIGHT + 1) * _SAI.HTF_INTERVAL_MS <= decision]
        if usable:
            swing = float(ctx.high[usable[-1]] if side > 0 else ctx.low[usable[-1]])
        setups.append({"i": int(i), "side": side, "wick": float(row["sai_sweep_wick"]),
                       "entry_price": float(row["sai_entry_price"]), "atr": float(atr[i]),
                       "setup_id": row["sai_setup_id"], "pools": [opposite, swing]})
    return setups


def tick_for(price, size_decimals):
    if not price > 0:
        raise ValueError(f"tick needs a positive price, got {price!r}")
    return float(max(10.0 ** (np.floor(np.log10(price)) - 4), 10.0 ** -(6 - int(size_decimals))))


def score(study, spec, base, manifests):
    reg = load_registry("futures")
    out = {"selection": {}, "datasets": {}}
    for interval, manifest in sorted(manifests.items()):
        grid = grid_params(spec, interval)
        train = {}
        for coin in spec["datasets"]["coins"]:
            ds = om.dataset_by_key(manifest, f"{coin} {interval}")
            frame, win, cov, flow_obs, htf_obs = _window_inputs(manifest, ds, "train")
            for params in grid:
                sig = candidate_signals(reg, frame, params, flow_obs, htf_obs)
                res = real_backtest(manifest, ds, win, sig, CANDIDATE, params, spec, 1.0)
                key = _canon(params)
                agg = train.setdefault(key, {"params": params, "net": 0.0, "dd": 0.0, "independent": 0})
                agg["net"] += float(res.get("total_return_pct") or 0.0)
                agg["dd"] = max(agg["dd"], abs(float(res.get("max_drawdown_pct") or 0.0)))
                agg["independent"] += independent_positions(res.get("trades") or [])
        floor = spec["minimum_independent_positions"]["train"]
        eligible = [a for a in train.values() if a["independent"] >= floor]
        chosen = sorted(eligible, key=lambda a: (-a["net"], a["dd"], _canon(a["params"])))[0] if eligible else None
        out["selection"][interval] = {"params": chosen["params"] if chosen else None,
                                      "train_net_return_pct": chosen["net"] if chosen else None,
                                      "eligible_combinations": len(eligible), "combinations": len(grid)}
        if chosen is None:
            continue
        params = chosen["params"]
        for coin in spec["datasets"]["coins"]:
            ds = om.dataset_by_key(manifest, f"{coin} {interval}")
            for window in ("train", "test"):
                frame, win, cov, flow_obs, htf_obs = _window_inputs(manifest, ds, window)
                usable = flow_usable_share(frame, flow_obs, params)
                in_window = ((frame["timestamp"] >= win["start_ms"]) & (frame["timestamp"] < win["end_ms"])).to_numpy()
                window_usable = flow_usable_share(om.slice_window(frame, win), flow_obs, params)
                entry = {"coverage": cov, "flow_usable": {k: v for k, v in window_usable.items() if k != "mask"}}
                sig = candidate_signals(reg, frame, params, flow_obs, htf_obs)
                eligible_mask = usable["mask"] & in_window
                entry["primary"] = {m: _summary(real_backtest(manifest, ds, win, sig, CANDIDATE, params, spec, m))
                                    for m in (spec["costs"]["base_multiplier"], spec["costs"]["stress_multiplier"])}
                off = dict(params, absorption_filter=False)
                entry["absorption_disabled"] = _summary(real_backtest(
                    manifest, ds, win, candidate_signals(reg, frame, off, flow_obs, htf_obs), CANDIDATE, off, spec, 1.0))
                for name in CONTROLS:
                    csig, cparams = causal_control_signals(reg, name, frame, eligible_mask)
                    entry[name] = _summary(real_backtest(manifest, ds, win, csig, name, cparams, spec, 1.0))
                wsig = om.slice_window(sig, win)
                wframe = om.slice_window(frame, win)
                bars = {"open": wframe["open"].to_numpy(), "high": wframe["high"].to_numpy(),
                        "low": wframe["low"].to_numpy(), "close": wframe["close"].to_numpy(),
                        "open_ms": wframe["timestamp"].to_numpy().astype(np.int64)}
                setups = setups_from(wsig, wframe, htf_obs, params)
                fund = om.load_funding(ds)
                funding = [] if fund is None else [(int(t), float(r)) for t, r in zip(fund["timestamp"], fund["rate"])]
                spec_costs = om.execution_spec(manifest, ds, 1.0)
                tick = tick_for(float(wframe["close"].iloc[-1]), ds["size_decimals"])
                research = {}
                for arm in arms.arm_matrix():
                    sim = arms.simulate(bars, setups, arm, costs=spec_costs, sz_decimals=ds["size_decimals"],
                                        tick=tick, capital=CAPITAL, funding=funding,
                                        interval_ms=manifest["interval_ms"])
                    research[sim["arm"]] = {k: v for k, v in sim.items() if k != "trades"}
                    if arm == arms.REFERENCE_ARM:
                        research["_reference_entries"] = [(t["fill_bar"], t["side"]) for t in sim["trades"]]
                ref_entries = research.pop("_reference_entries", [])
                real = real_backtest(manifest, ds, win, sig, CANDIDATE, params, spec, 1.0)
                real_entries = [(int(wframe.index.get_loc(pd.Timestamp(pos["entry_date"]))), pos["side"])
                                for pos in positions_of(real.get("trades"))]
                entry["research_arms"] = research
                entry["adapter_agreement"] = ref_entries == real_entries
                out["datasets"].setdefault(f"{coin} {interval}", {})[window] = entry
    return out


def verdict(spec, gates, scored):
    if gates:
        return "INCONCLUSIVE", ["missing or failed evidence gates: " + ", ".join(sorted({g["gate"] for g in gates}))]
    reasons = []
    floor = spec["coverage"]["floor"]
    need = spec["minimum_independent_positions"]["test"]
    for key, wins in sorted(scored["datasets"].items()):
        for name, entry in sorted(wins.items()):
            share = entry["flow_usable"]["share"]
            if share is None or share < floor:
                reasons.append(f"{key} {name}: flow-usable share {share} is below the {floor} floor")
        test = wins.get("test")
        if test is None or test["primary"][spec["costs"]["base_multiplier"]]["independent_positions"] < need:
            reasons.append(f"{key}: fewer than {need} independent held-out positions")
    for interval, sel in sorted(scored["selection"].items()):
        if sel["params"] is None:
            reasons.append(f"{interval}: no grid combination reached the train position floor")
    if reasons:
        return "INCONCLUSIVE", reasons
    base, stress = spec["costs"]["base_multiplier"], spec["costs"]["stress_multiplier"]
    tests = [w["test"] for w in scored["datasets"].values()]
    pooled = {m: sum(t["primary"][m]["net_return_pct"] for t in tests) for m in (base, stress)}
    measure = lambda s: s["net_return_pct"] - abs(s["max_drawdown_pct"])
    primary = sum(measure(t["primary"][base]) for t in tests)
    fails = []
    if not pooled[base] > 0 or not pooled[stress] > 0:
        fails.append(f"pooled held-out net return {pooled} is not positive at base and stress cost")
    for control in ("absorption_disabled",) + CONTROLS:
        other = sum(measure(t[control]) for t in tests)
        if not primary > other:
            fails.append(f"primary return minus drawdown {primary:.4f} does not beat {control} {other:.4f}")
    return ("FAIL", fails) if fails else ("PASS", ["every frozen gate and economic condition passed"])


def run(study_path=STUDY_MANIFEST, results_path=RESULTS, report_path=REPORT):
    study, spec, base = load_study(study_path)
    gates, manifests, recording = preflight(study, spec, base)
    proof = None
    if not gates:
        compared, mismatches = sealed_replay_proof(base, study, manifests)
        proof = {"compared": compared, "mismatches": mismatches}
        gates = [{"gate": "sealed_replay_proof", "path": study["sealed_payloads"]["path"], "reason": m}
                 for m in mismatches]
    scored = None
    if not gates:
        scored = score(study, spec, base, manifests)
    outcome, reasons = verdict(spec, gates, scored)
    results = {
        "schema": "sweep_absorption_ifvg_results/1",
        "study": spec["study"],
        "candidate": CANDIDATE,
        "verdict": outcome,
        "verdict_reasons": reasons,
        "source": {"choice": spec["source"]["choice"], "label": spec["source"]["label"],
                   "contract": spec["source"]["contract"],
                   "recording_run_id": (recording or {}).get("report", {}).get("run_id")},
        "spec_sha256": study["spec"]["sha256"],
        "required_datasets": [f"{c} {i}" for i in spec["datasets"]["intervals"] for c in spec["datasets"]["coins"]],
        "gates": sorted(gates, key=_canon),
        "coverage_rule": spec["coverage"],
        "minimum_independent_positions": spec["minimum_independent_positions"],
        "support_bars": {i: max_support_bars(spec, i) for i in spec["datasets"]["intervals"]},
        "arm_matrix": [arms.arm_label(a) for a in arms.arm_matrix()],
        "controls": ["absorption_disabled"] + list(CONTROLS),
        "economic_scoring_ran": scored is not None,
        "sealed_replay_proof": proof,
        "scored": scored,
        "source_files": {rel: _sha(os.path.join(_REPO, rel)) for rel in SOURCE_FILES},
    }
    blob = json.dumps(results, indent=2, sort_keys=True, default=str) + "\n"
    with open(results_path, "w") as fh:
        fh.write(blob)
    with open(report_path, "w") as fh:
        fh.write(render_report(results, spec))
    return results


def render_report(results, spec):
    lines = [
        "# sweep_absorption_ifvg study (#2010)", "",
        f"**Verdict: {results['verdict']}.** Live use stays refused for every verdict; research is suggest-only.", "",
        "## Summary", "",
        "`sweep_absorption_ifvg` is a production-refused research candidate: a liquidity sweep of the UTC session "
        "low (high), buyer (seller) absorption in session-anchored aggressor volume, a displacement close through "
        "the inverse fair value gap and a retest of its consequent encroachment, gated by one-hour swing structure. "
        "The specification (`candidate_spec.json`) was frozen before any window was read.", "",
        f"Economic scoring ran: {'yes' if results['economic_scoring_ran'] else 'no'}.", "",
        "## Verdict reasons", "",
    ]
    lines += [f"- {r}" for r in results["verdict_reasons"]] + [""]
    lines += ["## Evidence gates", "", "| Gate | Item | Reason |", "| --- | --- | --- |"]
    for g in results["gates"]:
        lines.append(f"| {g['gate']} | {g.get('dataset') or g.get('path') or ''} | {g['reason']} |")
    if not results["gates"]:
        lines.append("| none | | every gate passed |")
    lines += ["", "## Source", "",
              f"Source {results['source']['choice']}: {results['source']['label']}.", "",
              f"Reason: {spec['source']['reason']}", "",
              "## Frozen protocol", "",
              f"- Required datasets: {', '.join(results['required_datasets'])} (no dataset is dropped to reach a verdict).",
              f"- Windows: train {spec['windows']['train']['start']} to {spec['windows']['train']['end']}, "
              f"test {spec['windows']['test']['start']} to {spec['windows']['test']['end']}.",
              f"- Flow-usable floor {spec['coverage']['floor']} per dataset and window; denominator: "
              f"{spec['coverage']['denominator']}.",
              f"- Independent positions: at least {spec['minimum_independent_positions']['test']} per held-out "
              f"dataset ({spec['independent_position']})",
              f"- Support horizon (decision bars at the widest grid point): "
              + ", ".join(f"{k}: {v}" for k, v in sorted(results["support_bars"].items())) + ".",
              f"- Arms: {', '.join(results['arm_matrix'])}; primary arm: {spec['execution']['primary_arm']}.",
              f"- Controls: {', '.join(results['controls'])} ({spec['controls']['price_only_rule']}).",
              f"- Costs: fees, spread, slippage, lot size and the 10 USD minimum from the pinned manifests; "
              f"stress multiplier {spec['costs']['stress_multiplier']}; funding {spec['costs']['funding']}.",
              "- Limit fills from candles are a queue assumption; a one-minute volume aggregate proves neither "
              "trade-through order nor available quantity.",
              "- Requested risk, initial stop risk and realized loss are reported separately; the Kelly fraction is "
              "a diagnostic only.", ""]
    if results["scored"]:
        lines += ["## Results", "", "```json", json.dumps(results["scored"], indent=2, sort_keys=True, default=str),
                  "```", ""]
    lines += ["## Reproduce", "", "```bash",
              "uv run --no-sync python backtest/candidates/sweep_absorption_ifvg_2010/run_study.py run",
              "uv run --no-sync python backtest/candidates/sweep_absorption_ifvg_2010/run_study.py check", "```", "",
              "`check` reruns the study into a scratch copy and compares it byte for byte with the committed "
              "`results.json` and `REPORT.md`.", ""]
    return "\n".join(lines)


def check():
    import tempfile
    with tempfile.TemporaryDirectory() as tmp:
        res, rep = os.path.join(tmp, "results.json"), os.path.join(tmp, "REPORT.md")
        run(results_path=res, report_path=rep)
        for fresh, committed in ((res, RESULTS), (rep, REPORT)):
            with open(fresh, "rb") as a, open(committed, "rb") as b:
                if a.read() != b.read():
                    raise SystemExit(f"{committed} does not reproduce")
    print("results.json and REPORT.md reproduce")


def main(argv=None):
    ap = argparse.ArgumentParser(description="sweep_absorption_ifvg frozen study (#2010)")
    ap.add_argument("command", choices=("run", "check"))
    args = ap.parse_args(argv)
    if args.command == "run":
        out = run()
        print(json.dumps({"verdict": out["verdict"], "gates": len(out["gates"])}))
    else:
        check()


if __name__ == "__main__":
    main()

import importlib.util
import json
import os
import subprocess
import sys

import numpy as np
import pandas as pd
import pytest

_HERE = os.path.dirname(os.path.abspath(__file__))
_REPO = os.path.abspath(os.path.join(_HERE, "..", ".."))
_TOOLS = os.path.join(_REPO, "shared_tools")
if _TOOLS not in sys.path:
    sys.path.insert(0, _TOOLS)

from atr import ensure_atr_indicator
from backtester import Backtester
from close_registry_loader import evaluate as close_evaluate
from closed_bar import FRAME_DECISION_TIME_ATTR, FRAME_INTERVAL_ATTR, FRAME_TIMESTAMP_KIND_ATTR
from registry_loader import load_registry
from strategy_composition import evaluate_open_close, finalize_decision
from trade_flow_replay import CONTRACT, PAYLOAD_CONTRACT_KEYS, payload_from_series

NAME = "sweep_absorption_ifvg"
STUDY_DIR = os.path.join(_REPO, "backtest", "candidates", "sweep_absorption_ifvg_2010")


def _load(name, path):
    spec = importlib.util.spec_from_file_location(name, path)
    mod = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(mod)
    return mod


scn = _load("_sai_scenario", os.path.join(STUDY_DIR, "scenario.py"))
_SAI = _load("_sai_module_for_tests", os.path.join(_REPO, "shared_strategies", "open", "sweep_absorption_ifvg.py"))
DEFAULT_PARAMS = dict(_SAI.DEFAULT_PARAMS)
BAR = scn.STEP
COLS = ["signal", "sai_reason", "sai_setup_id", "sai_flow_sha256", "sai_entry_price", "sai_absorption_ratio"]


def _reg():
    return load_registry("futures")


def _apply(bars, intervals, params=None, flow=True, htf=True, attrs=None, cutoff=None, htf_bars=None):
    df = scn.frame(bars)
    if attrs:
        df.attrs.update(attrs)
    p = dict(params or {})
    if flow:
        p["trade_flow_observations"] = scn.flow_payload(intervals, cutoff=cutoff)
    if htf:
        p["htf_candles"] = scn.htf_payload(bars if htf_bars is None else htf_bars, bars[-1]["T"] + 1)
    return _reg().apply_strategy(NAME, df, p)


def _row(day, bar):
    return scn.bar_index(day, bar)


def _same(a, b):
    return [("nan" if isinstance(x, float) and np.isnan(x) else x) for x in a] == \
           [("nan" if isinstance(x, float) and np.isnan(x) else x) for x in b]


def test_registry_evaluator_enters_both_directions_once_per_setup_with_frozen_evidence():
    bars, intervals = scn.build(["flat", "long", "short", "long"])
    out = _apply(bars, intervals)
    entries = out.index[out["signal"] != 0]
    assert [int(out.loc[t, "signal"]) for t in entries] == [1, -1, 1]
    for day, side in ((1, 1), (2, -1), (3, 1)):
        i = _row(day, scn.RETEST_BAR)
        assert int(out["signal"].iloc[i]) == side
        assert out["sai_reason"].iloc[i] == ("entry_long" if side == 1 else "entry_short")
        assert out["sai_reason"].iloc[_row(day, scn.CONFIRM_BAR)] == ("armed_long" if side == 1 else "armed_short")
        assert out["sai_absorption_ratio"].iloc[i] > DEFAULT_PARAMS["absorption_threshold"]
        assert out["sai_htf_bias"].iloc[i] == side
        lo, hi = out["sai_gap_bottom"].iloc[i], out["sai_gap_top"].iloc[i]
        assert out["sai_entry_price"].iloc[i] == pytest.approx((lo + hi) / 2)
        assert out["sai_confirm_ms"].iloc[i] < out["sai_deadline_ms"].iloc[i] - BAR
    ids = [out["sai_setup_id"].iloc[_row(d, scn.RETEST_BAR)] for d in (1, 2, 3)]
    assert len(set(ids)) == 3 and all(i.startswith("BTC:") for i in ids)


def test_hard_filters_hold_with_their_reason():
    for variant, reason in (("new_delta_low", "absorption_new_delta_extreme"),
                            ("weak_displacement", "displacement_weak")):
        bars, intervals = scn.build(["flat", "long", "short"], variants={1: variant, 2: variant})
        out = _apply(bars, intervals)
        assert not (out["signal"] != 0).any(), variant
        assert reason in set(out["sai_reason"].iloc[_row(1, 96):_row(1, 110)]), variant
        assert reason in set(out["sai_reason"].iloc[_row(2, 96):_row(2, 110)]), variant
    bars, intervals = scn.build(["flat", "long", "short"])
    for params, reason in (({"min_fvg_atr": 5.0}, "no_gap"), ({"min_displacement_atr": 5.0}, "displacement_weak"),
                           ({"absorption_threshold": 0.5}, "absorption_failed")):
        out = _apply(bars, intervals, params)
        assert not (out["signal"] != 0).any(), params
        assert reason in set(out["sai_reason"]), params
    off = _apply(*scn.build(["flat", "long"], variants={1: "new_delta_low"}), {"absorption_filter": False})
    assert int(off["signal"].iloc[_row(1, scn.RETEST_BAR)]) == 1


def test_missing_late_and_revoked_flow_or_htf_input_holds_with_a_reason():
    bars, intervals = scn.build(["flat", "long"])
    i = _row(1, scn.RETEST_BAR)
    assert _apply(bars, intervals, flow=False)["sai_reason"].iloc[i] == "trade_flow_unavailable"
    assert _apply(bars, intervals, htf=False)["sai_reason"].iloc[i] == "htf_unavailable"
    confirm_deadline = bars[_row(1, scn.CONFIRM_BAR)]["t"] + 2 * BAR
    late = [dict(iv, finalized_ms=confirm_deadline) if iv["start_ms"] == bars[_row(1, 30)]["t"] else iv
            for iv in intervals]
    out = _apply(bars, late)
    assert out["sai_reason"].iloc[_row(1, scn.CONFIRM_BAR)] == "flow_unavailable"
    assert int(out["signal"].iloc[i]) == 0
    revoke_at = bars[_row(1, scn.RETEST_BAR)]["t"] + BAR - 1
    revoked = [dict(iv, status="invalid", invalid_ms=revoke_at) if iv["start_ms"] == bars[_row(1, 40)]["t"] else iv
               for iv in intervals]
    out = _apply(bars, revoked)
    assert out["sai_reason"].iloc[_row(1, scn.CONFIRM_BAR)] == "armed_long"
    assert out["sai_reason"].iloc[i] == "setup_cleared_flow" and int(out["signal"].iloc[i]) == 0
    missing = [iv for iv in intervals if iv["start_ms"] != bars[_row(1, 50)]["t"]]
    assert _apply(bars, missing)["sai_reason"].iloc[_row(1, scn.CONFIRM_BAR)] == "flow_unavailable"
    mismatch = [dict(bar, v=bar["v"] + 1.0) if k == _row(1, 60) else bar for k, bar in enumerate(bars)]
    assert _apply(mismatch, intervals)["sai_reason"].iloc[_row(1, scn.CONFIRM_BAR)] == "flow_volume_mismatch"


def test_decision_time_is_required_at_the_last_row():
    bars, intervals = scn.build(["flat", "long"])
    cut = bars[:_row(1, scn.RETEST_BAR) + 1]
    boundary = cut[-1]["t"] + BAR
    out = _apply(cut, intervals, attrs={FRAME_DECISION_TIME_ATTR: {}})
    assert out["sai_reason"].iloc[-1] == "decision_time_missing" and int(out["signal"].iloc[-1]) == 0
    timed = {FRAME_DECISION_TIME_ATTR: {"cutoff_ms": boundary + 1_000, "boundary_ms": boundary},
             FRAME_INTERVAL_ATTR: BAR, FRAME_TIMESTAMP_KIND_ATTR: "open"}
    assert int(_apply(cut, intervals, attrs=timed)["signal"].iloc[-1]) == 1
    stale = {FRAME_DECISION_TIME_ATTR: {"cutoff_ms": boundary + BAR, "boundary_ms": boundary}}
    assert _apply(cut, intervals, attrs=stale)["sai_reason"].iloc[-1] == "decision_bar_stale"


def test_reconstruction_agrees_across_prefixes_windows_and_separate_workers():
    bars, intervals = scn.build(["flat", "long", "short"])
    full = _apply(bars, intervals)
    support = _SAI.support_for_params({})["decision_bars"]
    for day in (1, 2):
        for k in (scn.SWEEP_BAR, scn.CONFIRM_BAR, scn.CONFIRM_BAR + 1, scn.RETEST_BAR, scn.RETEST_BAR + 1):
            i = _row(day, k)
            prefix = _apply(bars[:i + 1], intervals)
            assert _same(prefix[COLS].iloc[-1].tolist(), full[COLS].iloc[i].tolist()), (day, k)
            window = _apply(bars[i + 1 - 200:i + 1], intervals, htf_bars=bars)
            assert _same(window[COLS].iloc[-1].tolist(), full[COLS].iloc[i].tolist()), (day, k)
    retest = _row(1, scn.RETEST_BAR)
    session_end = _row(1, 96)
    first_needed = session_end - support + 1 + _SAI.ALIGNMENT_ROWS
    edge = _apply(bars[first_needed - _SAI.ALIGNMENT_ROWS:retest + 1], intervals, htf_bars=bars)
    assert int(edge["signal"].iloc[-1]) == 1
    exact = _apply(bars[session_end - 96:retest + 1], intervals, htf_bars=bars)
    assert int(exact["signal"].iloc[-1]) == 1, "the session start at the retained-history edge must still decide"
    short = _apply(bars[session_end - 95:retest + 1], intervals, htf_bars=bars)
    assert short["sai_reason"].iloc[-1] == "insufficient_history" and int(short["signal"].iloc[-1]) == 0
    script = (
        "import json,sys,importlib.util\n"
        f"sys.path.insert(0,{os.path.join(_REPO, 'backtest')!r});sys.path.insert(0,{_TOOLS!r})\n"
        "from registry_loader import load_registry\n"
        f"s=importlib.util.spec_from_file_location('s',{os.path.join(STUDY_DIR, 'scenario.py')!r})\n"
        "m=importlib.util.module_from_spec(s);s.loader.exec_module(m)\n"
        "b,iv=m.build(['flat','long','short'])\n"
        "n=int(sys.argv[1])\n"
        "out=load_registry('futures').apply_strategy('sweep_absorption_ifvg',m.frame(b[:n]),"
        "{'trade_flow_observations':m.flow_payload(iv),'htf_candles':m.htf_payload(b)})\n"
        "r=out.iloc[-1];print(json.dumps([int(r['signal']),r['sai_reason'],r['sai_setup_id'],r['sai_flow_sha256']]))\n"
    )
    for i in (_row(1, scn.RETEST_BAR), _row(2, scn.RETEST_BAR)):
        proc = subprocess.run([sys.executable, "-c", script, str(i + 1)], capture_output=True, text=True, check=True)
        r = full.iloc[i]
        assert json.loads(proc.stdout) == [int(r["signal"]), r["sai_reason"], r["sai_setup_id"], r["sai_flow_sha256"]]


def test_setup_lifecycle_consumption_expiry_session_reset_and_bounds():
    bars, intervals = scn.build(["flat", "long", "long"])
    out = _apply(bars, intervals)
    i = _row(1, scn.RETEST_BAR)
    assert int(out["signal"].iloc[i]) == 1
    retouch = [dict(b) for b in bars]
    j = _row(1, scn.RETEST_BAR + 2)
    retouch[j]["l"] = min(retouch[j]["l"], out["sai_entry_price"].iloc[i] - 0.01)
    again = _apply(retouch, intervals)
    assert int(again["signal"].iloc[j]) == 0 and int((again["signal"].iloc[i:_row(2, 0)] != 0).sum()) == 1
    assert int(out["signal"].iloc[_row(2, scn.RETEST_BAR)]) == 1
    assert out["sai_setup_id"].iloc[_row(2, scn.RETEST_BAR)] != out["sai_setup_id"].iloc[i]
    no_retest = [dict(b) for b in bars]
    for k in range(scn.RETEST_BAR, scn.RETEST_BAR + 20):
        b = no_retest[_row(1, k)]
        b["l"] = max(b["l"], 98.95)
        b["o"], b["c"] = max(b["o"], b["l"]), max(b["c"], b["l"])
        b["h"] = max(b["h"], b["o"], b["c"])
    expired = _apply(no_retest, intervals, {"setup_expiry": 6})
    reasons = list(expired["sai_reason"].iloc[_row(1, scn.CONFIRM_BAR):_row(1, scn.CONFIRM_BAR + 7)])
    assert reasons[0] == "armed_long" and reasons[6] == "setup_expired"
    assert not (expired["signal"].iloc[_row(1, 0):_row(2, 0)] != 0).any()
    assert out["sai_reason"].iloc[_row(2, 0)] == "in_session"
    for params in ({"sweep_window": 57, "setup_expiry": 36}, {"expected_interval_ms": 60_000},
                   {"session_start_hour_utc": 8, "session_end_hour_utc": 8}):
        with pytest.raises(ValueError):
            _reg().validate_params(NAME, params)
        with pytest.raises(ValueError):
            _apply(bars, intervals, params)


def test_htf_bias_is_truncation_invariant_at_hour_boundaries_and_uses_only_closed_hours():
    bars, intervals = scn.build(["flat", "long", "short"])
    full = _apply(bars, intervals)
    for k in (96, 108, 120, 132):
        i = _row(1, k)
        cut = bars[:i]
        trunc = _reg().apply_strategy(NAME, scn.frame(cut), {"trade_flow_observations": scn.flow_payload(intervals),
                                                              "htf_candles": scn.htf_payload(bars, cut[-1]["T"] + 1)})
        assert _same(trunc[COLS].iloc[-1].tolist(), full[COLS].iloc[i - 1].tolist()), k
    ctx = _SAI.parse_htf_candles(scn.htf_payload(bars))
    confirm_deadline = bars[_row(1, scn.CONFIRM_BAR)]["t"] + BAR
    assert _SAI.htf_bias_at(ctx, confirm_deadline) == 1
    future = scn.htf_payload(bars)
    boundary = confirm_deadline // scn.HOUR * scn.HOUR
    future["rows"] = [r if r[0] < boundary else [r[0], r[1] * 2, r[2] * 2, r[3] * 2, r[4] * 2, r[5]]
                      for r in future["rows"]]
    assert _SAI.htf_bias_at(_SAI.parse_htf_candles(future), confirm_deadline) == 1
    stale = scn.htf_payload(bars, boundary - scn.HOUR)
    assert _SAI.htf_bias_at(_SAI.parse_htf_candles(stale), confirm_deadline) is None
    native = scn.htf_payload(bars)
    native["timestamp_kind"] = "native_close_inclusive"
    native["rows"] = [[r[0] + scn.HOUR - 1] + r[1:] for r in native["rows"]]
    assert _SAI.htf_bias_at(_SAI.parse_htf_candles(native), confirm_deadline) == 1


def test_sealed_and_replayed_flow_give_equal_decisions_at_the_same_cutoff():
    bars, intervals = scn.build(["flat", "long", "short"])
    series = {"coin": "BTC", **{k: CONTRACT[k] for k in PAYLOAD_CONTRACT_KEYS},
              "intervals": [dict(iv) for iv in intervals], "economic_evidence": True, "refusals": []}
    for day in (1, 2):
        for k in (scn.CONFIRM_BAR, scn.RETEST_BAR):
            i = _row(day, k)
            cutoff = bars[i]["t"] + BAR + 10_000
            frame = scn.frame(bars[:i + 1])
            sealed = scn.flow_payload([iv for iv in intervals if iv["finalized_ms"] <= cutoff], cutoff=cutoff)
            replayed = payload_from_series(series, BAR, cutoff)
            htf = scn.htf_payload(bars, bars[i]["t"] + BAR)
            a = _reg().apply_strategy(NAME, frame, {"trade_flow_observations": sealed, "htf_candles": htf})
            b = _reg().apply_strategy(NAME, frame, {"trade_flow_observations": replayed, "htf_candles": htf})
            assert _same(a[COLS].iloc[-1].tolist(), b[COLS].iloc[-1].tolist()), (day, k)
    damaged = dict(series, economic_evidence=False, refusals=["changed segment hash"])
    out = _reg().apply_strategy(NAME, scn.frame(bars), {"trade_flow_observations": payload_from_series(damaged, BAR),
                                                         "htf_candles": scn.htf_payload(bars)})
    assert out["sai_reason"].iloc[_row(1, scn.RETEST_BAR)] == "trade_flow_unavailable"
    doubled = [dict(iv, total=str(float(iv["total"]) * 2), buy=str(float(iv["buy"]) * 2)) if iv["start_ms"] ==
               bars[_row(1, 20)]["t"] else iv for iv in intervals]
    out = _apply(bars, doubled)
    assert out["sai_reason"].iloc[_row(1, scn.CONFIRM_BAR)] in ("flow_volume_mismatch", "unsupported_trade_flow")
    assert int(out["signal"].iloc[_row(1, scn.RETEST_BAR)]) == 0


def test_composer_keeps_the_close_owner_executable_while_entry_inputs_are_missing():
    reg = _reg()
    bars, intervals = scn.build(["flat", "long"])
    i = _row(1, scn.RETEST_BAR)
    df = scn.frame(bars[:i + 1])
    tp = {"tp_tiers": [{"profit_pct": 0.01, "close_fraction": 1.0}]}
    mark = float(df["close"].iloc[-1])
    ctx = {"side": "long", "avg_cost": mark / 1.05, "entry_atr": 1.0, "current_quantity": 1.0, "initial_quantity": 1.0}
    for params, opened in (({"trade_flow_observations": scn.flow_payload(intervals),
                             "htf_candles": scn.htf_payload(bars)}, "long"),
                           ({"trade_flow_observations": scn.flow_payload(intervals)}, "none"),
                           ({"htf_candles": scn.htf_payload(bars)}, "none")):
        flat = finalize_decision(evaluate_open_close(reg.apply_strategy, reg.get_strategy, df, NAME, NAME,
                                                     ["tiered_tp_pct"], "", params=params,
                                                     close_evaluate=close_evaluate,
                                                     close_params_by_name={"tiered_tp_pct": tp}), "")
        assert flat["open_action"] == opened
        held = finalize_decision(evaluate_open_close(reg.apply_strategy, reg.get_strategy, df, NAME, NAME,
                                                     ["tiered_tp_pct"], "long", params=params, position_ctx=ctx,
                                                     close_evaluate=close_evaluate,
                                                     close_params_by_name={"tiered_tp_pct": tp}), "long")
        assert held["close_strategy"] == "tiered_tp_pct" and held["close_fraction"] == 1.0 and held["signal"] == -1


def test_backtester_fills_both_directions_at_the_next_open():
    bars, intervals = scn.build(["flat", "long", "short", "long", "short"])
    sig = ensure_atr_indicator(_apply(bars, intervals))
    entries = np.flatnonzero(sig["signal"].to_numpy())
    assert len(entries) == 4
    bt = Backtester(initial_capital=1000.0, platform="hyperliquid",
                    open_strategy={"name": NAME, "params": dict(DEFAULT_PARAMS)},
                    close_strategies=[{"name": "time_stop", "params": {"max_bars": 12}}],
                    stop_loss_atr_mult=1.0, direction="both", comparison_mode="approximate")
    res = bt.run(sig, strategy_name=NAME, symbol="BTC/USDT", timeframe="5m", save=False)
    trades = res["trades"]
    assert {t["side"] for t in trades} == {"long", "short"}
    for t in trades:
        pos = sig.index.get_loc(pd.Timestamp(t["entry_date"]))
        assert pos - 1 in entries
        assert t["entry_price"] == pytest.approx(float(sig["open"].iloc[pos]), rel=1e-3)


arms = _load("_sai_study_arms_for_tests", os.path.join(STUDY_DIR, "arms.py"))
COSTS = {"taker_fee_pct": 0.00045, "maker_fee_pct": 0.00015, "half_spread_pct": 0.00001, "slippage_pct": 0.0005,
         "min_notional_usd": 10.0}


def _bars(rows, start=scn.T0):
    return {"open": np.array([r[0] for r in rows], dtype=float), "high": np.array([r[1] for r in rows], dtype=float),
            "low": np.array([r[2] for r in rows], dtype=float), "close": np.array([r[3] for r in rows], dtype=float),
            "open_ms": np.array([start + k * BAR for k in range(len(rows))], dtype=np.int64)}


def _sim(rows, setups, arm, **kw):
    return arms.simulate(_bars(rows), setups, arm, costs=COSTS, sz_decimals=3, tick=0.01, capital=10_000.0,
                         interval_ms=BAR, **kw)


def test_research_arm_simulator_fills_stops_targets_and_sizes_by_its_frozen_rules():
    flat = [(100.0, 100.2, 99.8, 100.0)] * 3
    long_setup = {"i": 2, "side": 1, "wick": 98.0, "entry_price": 99.5, "atr": 1.0, "pools": [101.0, None]}
    rows = flat + [(100.0, 101.8, 99.9, 101.6), (101.6, 101.7, 99.0, 99.2), (99.2, 99.3, 97.0, 97.1)]
    market = _sim(rows, [long_setup], arms.REFERENCE_ARM)
    t = market["trades"][0]
    assert t["fill_bar"] == 3 and t["fill_px"] == pytest.approx(100.0 * (1 + 0.00051))
    assert [leg["kind"] for leg in t["legs"]] == ["tp1", "stop"]
    assert t["legs"][0]["qty"] == pytest.approx(t["qty"] / 2, abs=0.001)
    assert t["initial_stop_risk_usd"] <= t["requested_risk_usd"] + 1e-9
    assert t["qty"] == round(t["qty"], 3) and t["requested_risk_usd"] == pytest.approx(50.0)
    pool = _sim(rows, [long_setup], {"entry": "market", "stop": "structural", "target": "pool"})["trades"][0]
    assert pool["tp1"] == 101.0 and pool["initial_stop"] == pytest.approx(98.0 - 0.02)
    gap_rows = flat + [(100.0, 100.1, 99.9, 100.0), (97.0, 97.2, 96.5, 96.8)]
    gap = _sim(gap_rows, [long_setup], arms.REFERENCE_ARM)["trades"][0]
    assert gap["legs"][-1]["kind"] == "stop_gap" and gap["loss_beyond_initial_risk"]
    short_setup = {"i": 2, "side": -1, "wick": 102.0, "entry_price": 100.5, "atr": 1.0, "pools": [None, None]}
    short_rows = flat + [(100.0, 100.1, 98.2, 98.4), (98.4, 99.6, 98.3, 99.5), (99.5, 101.5, 99.4, 101.4)]
    short = _sim(short_rows, [short_setup], arms.REFERENCE_ARM)["trades"][0]
    assert short["side"] == "short" and [leg["kind"] for leg in short["legs"]] == ["tp1", "stop"]
    no_target = _sim(short_rows, [short_setup], {"entry": "market", "stop": "atr", "target": "pool"})
    assert no_target["trades"][0]["tp1"] is None
    limit_rows = flat + [(100.0, 100.3, 99.6, 100.2), (100.2, 100.4, 99.48, 99.9), (99.9, 100.0, 99.0, 99.1)]
    limit = _sim(limit_rows, [dict(long_setup, i=2)], {"entry": "ce_limit", "stop": "atr", "target": "tiered"})
    lt = limit["trades"][0]
    assert lt["fill_bar"] == 4 and lt["fill_kind"] == "limit_trade_through" and lt["fill_px"] == 99.5
    touch_only = flat + [(100.0, 100.3, 99.495, 100.2)] + [(100.2, 100.4, 100.0, 100.3)] * 8
    missed = _sim(touch_only, [long_setup], {"entry": "ce_limit", "stop": "atr", "target": "tiered"})
    assert missed["tallies"]["no_fill"] == 1 and missed["no_fill_share"] == 1.0 and not missed["trades"]
    retro = flat[:2] + [(100.0, 100.2, 99.0, 100.0)] + [(100.2, 100.4, 100.0, 100.3)] * 8
    assert not _sim(retro, [long_setup], {"entry": "ce_limit", "stop": "atr", "target": "tiered"})["trades"]
    gap_fill = flat + [(99.0, 99.8, 98.9, 99.6)] + [(99.6, 99.7, 99.5, 99.6)] * 2
    gf = _sim(gap_fill, [long_setup], {"entry": "ce_limit", "stop": "atr", "target": "tiered"})["trades"][0]
    assert gf["fill_kind"] == "limit_gap_open" and gf["fill_px"] == 99.0
    invalid = _sim(rows, [dict(long_setup, wick=100.5)], {"entry": "market", "stop": "structural", "target": "tiered"})
    assert invalid["tallies"]["stop_invalid"] == 1 and not invalid["trades"]
    tiny = arms.simulate(_bars(rows), [long_setup], arms.REFERENCE_ARM, costs=dict(COSTS, min_notional_usd=1e9),
                         sz_decimals=3, tick=0.01, capital=10_000.0, interval_ms=BAR)
    assert tiny["tallies"]["below_min_notional"] == 1
    funded = _sim(rows, [long_setup], arms.REFERENCE_ARM, funding=[(int(scn.T0 + 4 * BAR), 0.001)])
    assert funded["trades"][0]["funding"] < 0


def _write_csv_gz(path, frame):
    import gzip
    with gzip.open(path, "wt") as fh:
        frame.to_csv(fh, index=False)


def _synthetic_study(tmp, days):
    import gzip
    import shutil
    from trade_flow_replay import SERIES_SCHEMA, intervals_sha256
    bars, intervals = scn.build(days)
    data = os.path.join(tmp, "data")
    os.makedirs(data)
    cols = {"timestamp": [b["t"] for b in bars], "open": [b["o"] for b in bars], "high": [b["h"] for b in bars],
            "low": [b["l"] for b in bars], "close": [b["c"] for b in bars], "volume": [b["v"] for b in bars]}
    _write_csv_gz(os.path.join(data, "BTC_5m.csv.gz"), pd.DataFrame(cols))
    hours = scn.htf_rows(bars)
    _write_csv_gz(os.path.join(data, "BTC_1h.csv.gz"), pd.DataFrame(hours, columns=list(om_columns())))
    _write_csv_gz(os.path.join(data, "BTC_funding.csv.gz"),
                  pd.DataFrame({"timestamp": [h[0] for h in hours], "rate": [0.00001] * len(hours)}))
    series = {"schema": SERIES_SCHEMA, "coin": "BTC", **{k: CONTRACT[k] for k in PAYLOAD_CONTRACT_KEYS},
              "intervals": intervals, "intervals_sha256": intervals_sha256(intervals), "economic_evidence": True,
              "refusals": []}
    with gzip.open(os.path.join(data, "BTC_trade_flow.json.gz"), "wt") as fh:
        json.dump(series, fh)
    shutil.copyfile(os.path.join(_REPO, "backtest", "candidates", "cumulative_volume_delta_divergence_1638", "data",
                                 "venue_meta.json"), os.path.join(data, "venue_meta.json"))
    import offline_manifest as om
    day = scn.DAY

    def ref(name):
        return {"path": f"data/{name}", "sha256": om.sha256_file(os.path.join(data, name))}

    iso = lambda ms: pd.Timestamp(ms, unit="ms", tz="UTC").strftime("%Y-%m-%dT%H:%M:%SZ")
    manifest = {
        "schema": om.SCHEMA_V7, "study": "synthetic", "venue": "hyperliquid",
        "provenance": {"kind": "proxy", "label": "synthetic scenario candles and recorded-shape trade flow"},
        "interval": "5m", "warmup_bars": 150,
        "windows": {"train": {"start": iso(scn.T0 + 2 * day), "end": iso(scn.T0 + 5 * day)},
                    "test": {"start": iso(scn.T0 + 5 * day), "end": iso(scn.T0 + len(days) * day)}},
        "costs": {"taker_fee_pct": 0.00045, "maker_fee_pct": 0.00015, "slippage_bps": 5.0, "min_notional_usd": 10.0,
                  "min_notional_margin": 0.03},
        "venue_reference": {"meta": ref("venue_meta.json")},
        "datasets": [{"coin": "BTC", "symbol": "BTC/USDT", "size_decimals": 5, "half_spread_bps": 0.06,
                      "candles": ref("BTC_5m.csv.gz"), "funding": ref("BTC_funding.csv.gz"),
                      "trade_flow": ref("BTC_trade_flow.json.gz"), "htf_candles": ref("BTC_1h.csv.gz")}],
    }
    path = os.path.join(tmp, "offline_5m.json")
    with open(path, "w") as fh:
        json.dump(manifest, fh)
    return path


def om_columns():
    return ("timestamp", "open", "high", "low", "close", "volume")


def test_frozen_study_scores_a_pinned_v7_manifest_end_to_end(tmp_path):
    import offline_manifest as om
    study = _load("_sai_study_driver_for_tests", os.path.join(STUDY_DIR, "run_study.py"))
    path = _synthetic_study(str(tmp_path), ["flat", "flat", "long", "short", "long", "long", "short", "long"])
    manifest = om.load_manifest(path, verify=True)
    with open(os.path.join(STUDY_DIR, "candidate_spec.json")) as fh:
        spec = json.load(fh)
    spec = dict(spec, datasets={"coins": ["BTC"], "intervals": ["5m"], "htf_interval": "1h"},
                grid={k: [DEFAULT_PARAMS[k]] for k in spec["grid"]},
                minimum_independent_positions={"train": 1, "test": 1})
    scored = study.score({}, spec, str(tmp_path), {"5m": manifest})
    assert scored["selection"]["5m"]["params"]["expected_interval_ms"] == BAR
    entry = scored["datasets"]["BTC 5m"]
    for window in ("train", "test"):
        e = entry[window]
        assert e["flow_usable"]["share"] == 1.0 and e["flow_usable"]["denominator"] > 0
        assert e["coverage"]["htf_candles"]["complete"] and e["coverage"]["trade_flow"]["available"]
        assert e["primary"][1.0]["positions"] == 3, (window, e["primary"])
        assert e["primary"][1.0]["independent_positions"] == 3 and e["primary"][1.0]["turnover"] > 0
        assert e["adapter_agreement"] is True, window
        assert set(e["research_arms"]) == {arms.arm_label(a) for a in arms.arm_matrix()}
        assert e["research_arms"]["market+atr+tiered"]["positions"] == 3
        assert e["research_arms"]["ce_limit+atr+tiered"]["no_fill_share"] == 1.0
        for control in ("absorption_disabled", "amd_ifvg", "liquidity_sweeps"):
            assert control in e
    assert set(entry["train"]["primary"]) == {1.0, 2.0}
    outcome, reasons = study.verdict(spec, [], scored)
    assert outcome in ("PASS", "FAIL"), reasons
    gated, _ = study.verdict(spec, [{"gate": "source_b_recording", "reason": "missing"}], None)
    assert gated == "INCONCLUSIVE"
    thin = dict(spec, minimum_independent_positions={"train": 1, "test": 30})
    assert study.verdict(thin, [], scored)[0] == "INCONCLUSIVE"


def _sealed_envelope(bars, intervals, last, cutoff, tamper=False):
    every = bars[:last + 2]

    def frame(rows_bars, step):
        return {"rows": [[b["T"], b["o"], b["h"], b["l"], b["c"], b["v"]] for b in rows_bars],
                "required": len(rows_bars), "bars": len(rows_bars), "coverage_short": False,
                "first_open_ms": rows_bars[0]["t"], "last_open_ms": rows_bars[-1]["t"],
                "last_close_ms": rows_bars[-1]["T"], "last_recv_at_ms": rows_bars[-1]["t"], "source": "ws",
                "ready": True, "forming_bar_included": True,
                "timing": {"rule": "hyperliquid_native_close", "interval_ms": step,
                           "bars": [[b["t"], b["T"], True] for b in rows_bars]}}

    hours = [{"t": r[0], "T": r[0] + scn.HOUR - 1, "o": r[1], "h": r[2], "l": r[3], "c": r[4], "v": r[5]}
             for r in scn.htf_rows(every)][-40:]
    sealed = [dict(iv) for iv in intervals if iv["finalized_ms"] <= cutoff and iv["start_ms"] >= every[-210]["t"]]
    if tamper:
        k = next(n for n, iv in enumerate(sealed) if iv["start_ms"] == bars[last - scn.RETEST_BAR + 60]["t"])
        sealed[k] = dict(sealed[k], buy=sealed[k]["sell"], sell=sealed[k]["buy"])
    flow = scn.flow_payload(sealed, cutoff=cutoff)
    return {"v": 2, "market": {"version": 4, "snapshot_id": "sai/1", "generation": 1, "sealed_at_ms": cutoff,
                               "decision_cutoff_ms": cutoff, "feed_complete": True,
                               "frames": {"BTC|5m": frame(every[-210:], BAR), "BTC|1h": frame(hours, scn.HOUR)},
                               "trade_flows": {"BTC|trade_flow": flow}, "mids": {}}}


def test_study_sealed_versus_replayed_proof_accepts_equal_decisions_and_refuses_a_changed_payload(tmp_path):
    import offline_manifest as om
    study = _load("_sai_study_driver_for_proof", os.path.join(STUDY_DIR, "run_study.py"))
    days = ["flat", "flat", "long", "short", "long", "long", "short", "long"]
    path = _synthetic_study(str(tmp_path), days)
    manifest = om.load_manifest(path, verify=True)
    bars, intervals = scn.build(days)
    proof_dir = tmp_path / "data" / "sealed_payloads"
    proof_dir.mkdir()
    payloads = []
    for name, day, tamper in (("long.json", 2, False), ("short.json", 3, False), ("tampered.json", 2, True)):
        last = _row(day, scn.RETEST_BAR)
        cutoff = bars[last]["T"] + 1 + 2_000
        env = _sealed_envelope(bars, intervals, last, cutoff, tamper=tamper)
        (proof_dir / name).write_text(json.dumps(env))
        payloads.append({"path": f"data/sealed_payloads/{name}", "sha256": om.sha256_file(str(proof_dir / name)),
                         "coin": "BTC", "interval": "5m"})
    study_doc = {"sealed_payloads": {"path": "data/sealed_payloads/index.json"}}
    (proof_dir / "index.json").write_text(json.dumps({"payloads": payloads[:2]}))
    compared, mismatches = study.sealed_replay_proof(str(tmp_path), study_doc, {"5m": manifest})
    assert compared == 2 and mismatches == []
    (proof_dir / "index.json").write_text(json.dumps({"payloads": payloads}))
    compared, mismatches = study.sealed_replay_proof(str(tmp_path), study_doc, {"5m": manifest})
    assert compared == 3 and len(mismatches) == 1 and "tampered.json" in mismatches[0]


def _side_doc(rows):
    trades = [{"coin": coin, "trade": {"coin": coin, "side": side},
               "facts": {"taker_side": taker, "fill_matches_trade": match}} for coin, side, taker, match in rows]
    return {"segment": "segment-00001.ndjson", "evidence_version": "hyperliquid-trades-side-aggressor/1638-v1",
            "trades": trades}


def _run_side_evidence_study(tmp_path, docs):
    import offline_manifest as om
    study = _load("_sai_study_driver_for_side", os.path.join(STUDY_DIR, "run_study.py"))
    with open(os.path.join(STUDY_DIR, "study_manifest.json")) as fh:
        doc = json.load(fh)
    spec_path = os.path.join(STUDY_DIR, "candidate_spec.json")
    doc["spec"] = {"path": spec_path, "sha256": om.sha256_file(spec_path)}
    side_dir = tmp_path / "data" / "side_evidence"
    side_dir.mkdir(parents=True)
    refs = []
    for n, side in enumerate(docs):
        p = side_dir / f"side_{n}.json"
        p.write_text(json.dumps(side))
        refs.append({"path": f"data/side_evidence/side_{n}.json", "sha256": om.sha256_file(str(p))})
    doc["side_evidence"] = refs
    manifest_path = tmp_path / "study_manifest.json"
    manifest_path.write_text(json.dumps(doc))
    return study.run(str(manifest_path), str(tmp_path / "results.json"), str(tmp_path / "REPORT.md"))


def _side_gates(results):
    return [g for g in results["gates"] if g["gate"] == "side_evidence"]


def _agreeing(coin, n=30):
    return [(coin, "B" if k % 2 else "A", "B" if k % 2 else "A", True) for k in range(n)]


def test_side_evidence_content_gates_every_required_coin(tmp_path):
    good = _agreeing("BTC") + _agreeing("ETH") + _agreeing("HYPE")
    ok = _run_side_evidence_study(tmp_path / "ok", [_side_doc(good[:45]), _side_doc(good[45:])])
    assert _side_gates(ok) == [] and ok["side_evidence"]["verified"] is True

    flipped = good[:-1] + [("HYPE", "B", "A", True)]
    disagree = _run_side_evidence_study(tmp_path / "disagree", [_side_doc(flipped)])
    gates = _side_gates(disagree)
    assert [g["coin"] for g in gates] == ["HYPE"] and "1 side disagreements" in gates[0]["reason"]
    assert disagree["verdict"] == "INCONCLUSIVE" and disagree["economic_scoring_ran"] is False

    no_hype = _run_side_evidence_study(tmp_path / "no_hype", [_side_doc(_agreeing("BTC") + _agreeing("ETH"))])
    gates = _side_gates(no_hype)
    assert [g["coin"] for g in gates] == ["HYPE"] and gates[0]["reason"].startswith("HYPE: 0 taker-identified")
    assert no_hype["verdict"] == "INCONCLUSIVE" and no_hype["economic_scoring_ran"] is False

    thin = _run_side_evidence_study(tmp_path / "thin", [_side_doc(_agreeing("BTC", 29) + _agreeing("ETH")
                                                                  + _agreeing("HYPE"))])
    gates = _side_gates(thin)
    assert [g["coin"] for g in gates] == ["BTC"] and "29 taker-identified" in gates[0]["reason"]
    assert thin["verdict"] == "INCONCLUSIVE" and thin["economic_scoring_ran"] is False


def test_research_arms_are_withheld_when_the_reference_arm_disagrees_with_the_backtester(tmp_path, monkeypatch):
    import offline_manifest as om
    study = _load("_sai_study_driver_for_agreement", os.path.join(STUDY_DIR, "run_study.py"))
    path = _synthetic_study(str(tmp_path), ["flat", "flat", "long", "short", "long", "long", "short", "long"])
    manifest = om.load_manifest(path, verify=True)
    with open(os.path.join(STUDY_DIR, "candidate_spec.json")) as fh:
        spec = json.load(fh)
    spec = dict(spec, datasets={"coins": ["BTC"], "intervals": ["5m"], "htf_interval": "1h"},
                grid={k: [DEFAULT_PARAMS[k]] for k in spec["grid"]},
                minimum_independent_positions={"train": 1, "test": 1})
    real_simulate = study.arms.simulate

    def extra_reference_entry(bars, setups, arm, **kw):
        sim = real_simulate(bars, setups, arm, **kw)
        if arm == study.arms.REFERENCE_ARM and sim["trades"]:
            sim = dict(sim, trades=list(sim["trades"]) + [dict(sim["trades"][-1], fill_bar=10**6)])
        return sim

    monkeypatch.setattr(study.arms, "simulate", extra_reference_entry)
    scored = study.score({}, spec, str(tmp_path), {"5m": manifest})
    for window in ("train", "test"):
        e = scored["datasets"]["BTC 5m"][window]
        assert e["adapter_agreement"] is False
        refusal = e["research_arms"]
        assert refusal["scored"] is False and refusal["reason"] == "reference arm entries differ from the Backtester"
        assert set(refusal) == {"scored", "reason", "reference_entries", "backtester_entries"}
        assert len(refusal["reference_entries"]) == len(refusal["backtester_entries"]) + 1

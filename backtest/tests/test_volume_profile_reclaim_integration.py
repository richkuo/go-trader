import copy
import hashlib
import os
import subprocess
import sys
from fractions import Fraction

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
from optimizer import DEFAULT_PARAM_RANGES, generate_param_grid, walk_forward_optimize
from registry_loader import load_registry
from strategy_composition import evaluate_open_close, finalize_decision
from trade_flow_replay import _Flow, _Interval
from volume_profile_inputs import MINUTES, SESSION_MS, value_area
from volume_profile_replay import (ProfileReplay, load_profile_recording, payload_from_profile_series,
                                   refuse_volume_profile_strategy, sessions_sha256)

NAME = "volume_profile_reclaim"
BAR = 900_000
D0 = int(pd.Timestamp("2026-03-02T00:00:00Z").value // 1_000_000)
SEED = {"profile_bins": 4, "value_area_fraction": 0.6, "session_bounds": False, "expected_interval_ms": BAR}
HAND = [("100", 2), ("101", 1), ("102", 4), ("103", 1), ("104", 2), ("105", 3), ("106", 1), ("108", 2)]


def _reg():
    return load_registry("futures")


def _trade(tid, time_ms, px, sz="0.01"):
    units = int(round(float(sz) * 10 ** 8))
    return {"coin": "BTC", "tid": tid, "time": time_ms, "side": "B", "px": px, "sz": sz, "units": units,
            "hash": f"{tid:064x}", "buyer": "0xa", "seller": "0xb"}


def _replay_session(start, weights, flow=None, skip_minute=None):
    prof = ProfileReplay("BTC")
    tid = start // 1000
    for px, n in weights:
        for _ in range(n):
            tid += 1
            t = _trade(tid, start + (tid % MINUTES) * 60_000 + 10, px)
            prof.add_trade(t, hashlib.sha256(str(tid).encode()).digest(), start + (tid % MINUTES) * 60_000, start + 1)
    for m in range(MINUTES):
        if m == skip_minute:
            continue
        iv = _Interval(start + m * 60_000, 1)
        iv.status = "complete"
        prof.finalize_minute(iv, start + SESSION_MS + 5_000 if m == MINUTES - 1 else start + (m + 1) * 60_000 + 5_000)
    return prof


def _series(profiles, extra=None):
    from volume_profile_replay import _session_record
    sessions = [copy.deepcopy(_session_record(day)) for prof in profiles for _, day in sorted(prof.all_days().items())]
    sessions.extend(extra or [])
    sessions.sort(key=lambda s: s["session_start_ms"])
    import volume_profile_inputs as vpi
    from volume_profile_replay import PAYLOAD_KEYS, SERIES_SCHEMA
    return {"series_schema": SERIES_SCHEMA, "kind": "volume_profile", "coin": "BTC",
            **{k: vpi.CONTRACT[k] for k in PAYLOAD_KEYS}, "sessions": sessions, "sessions_sha256": sessions_sha256(sessions),
            "economic_evidence": True, "refusals": []}


def _frame(closes, start=D0 + SESSION_MS):
    n = len(closes)
    close = np.asarray(closes, dtype=float)
    idx = pd.date_range(pd.Timestamp(start, unit="ms"), periods=n, freq="15min")
    df = pd.DataFrame({"open": close, "high": close + 0.2, "low": close - 0.2, "close": close, "volume": np.full(n, 10.0)},
                      index=idx)
    df["timestamp"] = (idx.as_unit("ms").asi8).astype(np.int64)
    df.attrs["bar_interval_ms"] = BAR
    df.attrs["bar_timestamp_kind"] = "open"
    return df


def _run(df, series, cutoff=None, **over):
    payload = payload_from_profile_series(series, BAR, cutoff)
    return _reg().apply_strategy(NAME, df, {**SEED, **over, "volume_profile_observations": payload})


def test_registration_grid_validator_and_hiding():
    reg = _reg()
    entry = reg.STRATEGY_REGISTRY[NAME]
    assert entry["production_refused"] is True and entry["edge_status"] == "no_edge"
    assert entry["edge_ref"] == "backtest/candidates/volume_profile_reclaim_1665/REPORT.md"
    assert NAME not in reg.list_strategies() and NAME not in load_registry("spot").STRATEGY_REGISTRY
    assert reg._registry.STRATEGIES[NAME]["short_entries"] is True
    grid = generate_param_grid(DEFAULT_PARAM_RANGES[NAME])
    assert len(grid) == 9
    for params in grid:
        reg._registry.validate_params(NAME, {**params, "expected_interval_ms": BAR})
    for bad in ({"profile_bins": 1}, {"profile_bins": 513}, {"profile_bins": 2.5}, {"profile_bins": True},
                {"value_area_fraction": 0.0}, {"value_area_fraction": 1.0}, {"value_area_fraction": float("nan")},
                {"session_bounds": 1}, {"expected_interval_ms": 60_000}, {"expected_interval_ms": 14_400_000},
                {"bins": 64}):
        with pytest.raises(ValueError):
            reg._registry.validate_params(NAME, {**SEED, **bad})


def test_hand_profile_bins_ties_value_area_and_boundaries_through_the_replay():
    prof = _replay_session(D0, HAND)
    art = prof.days[D0].artifact
    assert [p for p, _ in art["prices"]] == ["100", "101", "102", "103", "104", "105", "106", "108"]
    assert art["volume"] == "0.16" and art["trades"] == 16 and art["available_ms"] == D0 + SESSION_MS + 5_000
    base = value_area(art, 4, Fraction(6, 10), False)
    assert base["volume"] == [3_000_000, 5_000_000, 5_000_000, 3_000_000]
    assert (base["poc"], base["val"], base["vah"]) == (1, 102, 106)
    wide = value_area(art, 4, Fraction(8, 10), False)
    assert (wide["val"], wide["vah"]) == (100, 106)
    bounds = value_area(art, 4, Fraction(6, 10), True)
    assert (bounds["val"], bounds["vah"]) == (100, 108)
    odd = value_area(art, 3, Fraction(1, 2), False)
    assert odd["width"] == Fraction(8, 3) and odd["volume"] == [7_000_000, 6_000_000, 3_000_000]
    dup = _replay_session(D0, HAND)
    dup.add_trade(_trade(10 ** 9, D0 + 10, "104"), b"\0" * 32, D0, D0 + 2)
    assert dup.days[D0].artifact["volume"] == "0.16"


def test_mirrored_reclaims_overshoot_boundary_and_ablation_are_deterministic():
    series = _series([_replay_session(D0, HAND)])
    closes = [104, 101, 103, 107, 105, 101, 107, 101, 102, 104, 101, 101.5, 104]
    out = _run(_frame(closes), series)
    reasons = list(out["vpr_reason"])
    assert reasons[0] == "session_start"
    assert reasons[2] == "entry_long" and out["signal"].iloc[2] == 1
    assert reasons[4] == "entry_short" and out["signal"].iloc[4] == -1
    assert reasons[6] == "opposite_boundary_overshoot" and reasons[7] == "opposite_boundary_overshoot"
    assert reasons[8] == "boundary_touch" and reasons[9] == "inside_value_area"
    assert reasons[11] == "outside_value_area" and reasons[12] == "entry_long"
    assert out["vpr_val"].iloc[2] == "102" and out["vpr_vah"].iloc[2] == "106" and out["vpr_side"].iloc[2] == "long"
    assert out["vpr_bins_json"].iloc[2] == "[3000000,5000000,5000000,3000000]"
    again = _run(_frame(closes), series)
    assert out["signal"].tolist() == again["signal"].tolist()
    abl = _run(_frame(closes), series, session_bounds=True)
    assert abl["vpr_val"].iloc[2] == "100" and abl["vpr_vah"].iloc[2] == "108"
    assert list(abl["signal"]) != list(out["signal"])


def test_sessions_gaps_availability_and_invalidation_are_dated():
    prof = _replay_session(D0, HAND)
    series = _series([prof])
    df = _frame([101, 103, 101, 103])
    gap = df.drop(df.index[1])
    assert list(_run(gap, series)["vpr_reason"])[1] == "bar_gap"
    late = copy.deepcopy(series)
    late["sessions"][0]["artifact"]["available_ms"] = D0 + SESSION_MS + BAR + 1
    from volume_profile_inputs import artifact_sha256
    late["sessions"][0]["artifact"]["sha256"] = artifact_sha256("BTC", late["sessions"][0]["artifact"])
    late["sessions_sha256"] = sessions_sha256(late["sessions"])
    reasons = list(_run(_frame([104, 101, 103, 101, 103]), late)["vpr_reason"])
    assert reasons[0] == "profile_pending" and reasons[1] == "profile_changed"
    assert reasons[2] == "entry_long" and reasons[4] == "entry_long"
    prof.invalidate_minute(D0 + 60_000, D0 + SESSION_MS + 3 * BAR + 10, "late_event")
    revoked = _series([prof])
    out = _run(_frame([104, 101, 103, 101, 103]), revoked)
    assert list(out["signal"])[:3] == [0, 0, 1] and list(out["vpr_reason"])[3:] == ["profile_invalidated"] * 2
    incomplete = _series([_replay_session(D0, HAND, skip_minute=600)])
    assert set(_run(_frame([104, 101, 103]), incomplete)["vpr_reason"]) == {"profile_pending"}
    assert set(_run(_frame([104, 101, 103], start=D0 + 3 * SESSION_MS), series)["vpr_reason"]) == {"profile_unavailable"}


def test_future_trades_and_later_evidence_cannot_change_past_decisions():
    p0, p1 = _replay_session(D0, HAND), _replay_session(D0 + SESSION_MS, [("100", 1), ("110", 9), ("111", 3)])
    df = _frame([104, 101, 103, 107, 105] * 30)
    full = _run(df, _series([p0, p1]))
    only = _run(df, _series([p0]))
    first_day = int(SESSION_MS // BAR)
    assert full["signal"].iloc[:first_day].tolist() == only["signal"].iloc[:first_day].tolist()
    p0.invalidate_minute(D0 + 60_000, D0 + SESSION_MS + 40 * BAR + 1, "conflicting_duplicate")
    revised = _run(df, _series([p0, p1]))
    assert revised["signal"].iloc[:40].tolist() == full["signal"].iloc[:40].tolist()
    assert set(revised["vpr_reason"].iloc[40:first_day]) == {"profile_invalidated"}
    assert full["signal"].iloc[40:first_day].abs().sum() > 0
    for k in range(3, 40):
        cutoff = int(df["timestamp"].iloc[k]) + BAR
        live = _run(df.iloc[:k + 2], _series([p0, p1]), cutoff=cutoff)
        assert live["signal"].iloc[k] == full["signal"].iloc[k]
        assert bool(live["vpr_carried"].iloc[k + 1]) and live["signal"].iloc[k + 1] == full["signal"].iloc[k]


def test_missing_or_malformed_profiles_hold_entries_and_the_composer_keeps_the_close_owner():
    series = _series([_replay_session(D0, HAND)])
    df = _frame([104, 101, 103])
    reg = _reg()
    tp = {"tp_tiers": [{"atr_multiple": 0.1, "close_fraction": 1.0}]}

    def decide(profile, side="", ctx=None):
        params = dict(SEED, volume_profile_observations=profile)
        ev = evaluate_open_close(reg.apply_strategy, reg.get_strategy, ensure_atr_indicator(df), NAME, NAME,
                                 ["tiered_tp_atr"], side, params=params, position_ctx=ctx, close_evaluate=close_evaluate,
                                 close_params_by_name={"tiered_tp_atr": tp})
        return finalize_decision(ev, side)

    assert decide(payload_from_profile_series(series, BAR))["open_action"] == "long"
    broken = copy.deepcopy(payload_from_profile_series(series, BAR))
    broken["sessions"][0]["artifact"]["prices"][0][1] = "0.03"
    missing = {"available": False, "kind": "volume_profile", "coin": "BTC", "reason": "not in the sealed snapshot"}
    for profile in (broken, missing):
        out = reg.apply_strategy(NAME, df, {**SEED, "volume_profile_observations": profile})
        assert set(out["signal"]) == {0} and set(out["vpr_reason"]) <= {"profile_malformed", "profile_input_missing"}
        mark = float(df["close"].iloc[-1])
        ctx = {"side": "long", "avg_cost": mark / 1.05, "entry_atr": 1.0, "current_quantity": 1.0, "initial_quantity": 1.0}
        held = decide(profile, "long", ctx)
        assert held["open_action"] == "none" and held["close_strategy"] == "tiered_tp_atr" and held["close_fraction"] == 1.0
        assert decide(profile)["open_action"] == "none"


def test_simulator_fills_next_open_and_the_close_owner_and_stop_exit():
    profiles = [_replay_session(D0 + i * SESSION_MS, HAND) for i in range(6)]
    closes = ([104, 101, 103, 103.5, 104, 105, 104.5, 107, 105, 104] + [104] * 86) * 5
    df = _frame(closes)
    sig = ensure_atr_indicator(_run(df, _series(profiles)))
    entries = np.flatnonzero(sig["signal"].to_numpy())
    assert len(entries) >= 8 and {1, -1} <= set(sig["signal"].iloc[entries])
    bt = Backtester(initial_capital=1000.0, platform="hyperliquid", open_strategy={"name": NAME, "params": dict(SEED)},
                    close_strategies=[{"name": "tiered_tp_atr", "params": {"tp_tiers": [{"atr_multiple": 2.0, "close_fraction": 1.0}]}}],
                    stop_loss_atr_mult=1.0, direction="both")
    res = bt.run(sig, strategy_name=NAME, symbol="BTC/USDT", timeframe="15m", save=False)
    assert {t["side"] for t in res["trades"]} == {"long", "short"}
    for t in res["trades"]:
        pos = sig.index.get_loc(pd.Timestamp(t["entry_date"]))
        assert int(sig["signal"].iloc[pos - 1]) == (1 if t["side"] == "long" else -1)
        assert t["entry_price"] == pytest.approx(df["open"].iloc[pos], rel=0.01)


def test_unsupported_consumers_refuse_before_scoring(tmp_path):
    df = _frame([104, 101, 103] * 40)
    with pytest.raises(ValueError, match="candle-only run cannot score it"):
        _reg().apply_strategy(NAME, df, dict(SEED))
    for consumer in ("walk-forward optimization", "the exit-policy A/B harness"):
        with pytest.raises(ValueError, match="executed-trade volume profiles"):
            refuse_volume_profile_strategy(NAME, consumer)
    with pytest.raises(ValueError, match="executed-trade volume profiles"):
        walk_forward_optimize(df, NAME, {"profile_bins": [64]}, n_splits=2, timeframe="15m", registry="futures")
    import eval_windows
    with pytest.raises(ValueError, match="offline_candle_manifest/v5"):
        eval_windows.run_leg(_reg(), NAME, dict(SEED), "BTC/USDT", "15m", ("2026-01-01", "2026-02-01"),
                             close_strategies=[{"name": "tiered_tp_atr", "params": {"tp_tiers": [{"atr_multiple": 2.0, "close_fraction": 1.0}]}}],
                             direction="both", stop_loss_atr_mult=1.0)
    for mode_args in (["--mode", "optimize"], ["--mode", "single"]):
        proc = subprocess.run([sys.executable, os.path.join(_REPO, "backtest", "run_backtest.py"), *mode_args,
                               "--strategy", NAME, "--registry", "futures", "--symbol", "BTC/USDT", "--timeframe", "15m"],
                              capture_output=True, text=True, cwd=_REPO, timeout=240)
        assert proc.returncode != 0 and "executed-trade volume profiles" in (proc.stdout + proc.stderr)
    import offline_manifest as om
    for path in ("backtest/candidates/volume_profile_reclaim_1665_exact_manifest.json",
                 "backtest/candidates/volume_profile_reclaim_1665_proxy_manifest.json"):
        manifest = om.load_manifest(os.path.join(_REPO, path))
        assert manifest["schema"] == om.SCHEMA_V5 and all(d["volume_profile"] for d in manifest["datasets"])
    import json
    with open(os.path.join(_REPO, "backtest/candidates/volume_profile_reclaim_1665_exact_manifest.json")) as fh:
        raw = json.load(fh)
    raw["schema"] = om.SCHEMA_V4
    old = os.path.join(_REPO, "backtest", "candidates", f".vpr-old-{os.getpid()}.json")
    try:
        with open(old, "w") as fh:
            json.dump(raw, fh)
        with pytest.raises(om.ManifestError, match="volume_profile needs schema"):
            om.load_manifest(old)
    finally:
        os.remove(old)


def test_manifest_leg_scores_from_the_pinned_series_and_holds_without_a_complete_session():
    import eval_windows
    import offline_manifest as om
    manifest = om.load_manifest(os.path.join(_REPO, "backtest/candidates/volume_profile_reclaim_1665_proxy_manifest.json"))
    cand_close = [{"name": "tiered_tp_atr", "params": {"tp_tiers": [{"atr_multiple": 2.0, "close_fraction": 1.0}]}}]
    leg = eval_windows.run_leg(_reg(), NAME, {"profile_bins": 64, "value_area_fraction": 0.7, "session_bounds": False},
                               "BTC", "15m", (None, None), close_strategies=cand_close, direction="both", stop_loss_atr_mult=1.0,
                               manifest_ctx={"manifest": manifest, "window": "test", "cost_multiplier": 1.0})
    cov = leg["manifest"]["volume_profile_coverage"]
    assert leg["trades"] > 0 and cov["complete_prior_sessions"] == cov["needed_prior_sessions"] == 14
    assert not leg.get("funding_incomplete")
    exact = om.load_manifest(os.path.join(_REPO, "backtest/candidates/volume_profile_reclaim_1665_exact_manifest.json"))
    held = eval_windows.run_leg(_reg(), NAME, {"profile_bins": 64, "value_area_fraction": 0.7, "session_bounds": False},
                                "BTC", "15m", (None, None), close_strategies=cand_close, direction="both", stop_loss_atr_mult=1.0,
                                manifest_ctx={"manifest": exact, "window": "test", "cost_multiplier": 1.0})
    assert held["trades"] == 0 and held["manifest"]["volume_profile_coverage"]["complete_prior_sessions"] == 0


def test_committed_recording_replays_to_the_pinned_profile_series():
    rec = os.path.join(_REPO, "backtest", "candidates", "cumulative_volume_delta_divergence_1638", "data", "recording")
    loaded = load_profile_recording(rec)
    import json
    with open(os.path.join(_REPO, "backtest/candidates/volume_profile_reclaim_1665/data/import_report.json")) as fh:
        report = json.load(fh)
    for coin in ("BTC", "ETH"):
        series = loaded["series"][coin]
        assert series["sessions_sha256"] == report["exact"][coin]["sessions_sha256"]
        assert series["complete_sessions"] == 0
        assert [s["reason_code"] for s in series["sessions"]] == ["minute_invalid"]
    flow = _Flow("BTC", 60_000)
    assert flow.profile is None

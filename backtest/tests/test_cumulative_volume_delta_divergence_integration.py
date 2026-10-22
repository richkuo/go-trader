import contextlib
import os
import sys
from decimal import Decimal

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
from closed_bar import TIMESTAMP_KIND_NATIVE_CLOSE, declare_frame_cadence
from optimizer import DEFAULT_PARAM_RANGES, generate_param_grid
from registry_loader import load_registry
from strategy_composition import (
    GATE_MODE_LIVE,
    GATE_MODE_MISSING,
    GATE_MODE_PAPER,
    Acknowledgement,
    GateMode,
    admit_configured_strategies,
    evaluate_open_close,
    finalize_decision,
)
from trade_flow_replay import CONTRACT, PAYLOAD_CONTRACT_KEYS, payload_from_series

NAME = "cumulative_volume_delta_divergence"
BAR = 300_000
T0 = int(pd.Timestamp("2026-01-01T00:00:00Z").value // 1_000_000)
SEED = {"left_span": 2, "right_span": 2, "min_separation": 2, "max_separation": 36, "delta_threshold": 0.05,
        "setup_expiry": 4, "session_hours": 4, "expected_interval_ms": BAR, "delta_confirmation": True}
P1, P2 = 4, 10
CONFIRM = P2 + 2


def _reg():
    return load_registry("futures")


@contextlib.contextmanager
def _case(label):
    try:
        yield
    except AssertionError as exc:
        raise AssertionError(f"case {label}: {exc}") from exc
    except pytest.fail.Exception as exc:
        pytest.fail(f"case {label}: {exc}")
    except Exception as exc:
        exc.add_note(f"case {label}")
        raise


def _dec(value) -> str:
    text = format(Decimal(value).normalize(), "f")
    return text.rstrip("0").rstrip(".") if "." in text else text


def _scenario(n=40, side="long", buy_frac="0.6", entry_at=CONFIRM + 2):
    low = np.full(n, 100.0)
    high = np.full(n, 101.0)
    close = np.full(n, 100.5)
    if side == "long":
        low[P1], low[P2] = 95.0, 94.0
        high[P1], high[P2] = 96.0, 95.0
        close[P1], close[P2] = 95.5, 94.5
        if entry_at is not None:
            high[entry_at], close[entry_at] = 102.0, 101.5
    else:
        high[P1], high[P2] = 106.0, 107.0
        low[P1], low[P2] = 105.0, 106.0
        close[P1], close[P2] = 105.5, 106.5
        if entry_at is not None:
            low[entry_at], close[entry_at] = 99.0, 99.5
    fracs = [Decimal("0.5")] * n
    for i in range(P1 + 1, P2 + 1):
        fracs[i] = Decimal(buy_frac)
    return {"low": low, "high": high, "close": close, "fracs": fracs}


def _build(sc, unit="0.2", mutate=None):
    n = len(sc["low"])
    idx = pd.date_range(pd.Timestamp(T0, unit="ms"), periods=n, freq="5min")
    intervals, volumes = [], []
    for i in range(n):
        total = Decimal(0)
        for k in range(5):
            start = T0 + i * BAR + k * 60_000
            u = Decimal(unit)
            buy = (u * sc["fracs"][i]).quantize(Decimal("0.0001"))
            sell = u - buy
            total += u
            intervals.append({"start_ms": start, "end_ms": start + 60_000, "buy": _dec(buy), "sell": _dec(sell),
                              "total": _dec(u), "trades": 3, "records_hash": f"{start:064x}", "session": 1,
                              "finalized_ms": start + 66_000, "status": "complete", "invalid_ms": None, "reason": ""})
        volumes.append(float(total))
    if mutate:
        mutate(intervals, volumes)
    df = pd.DataFrame({"open": sc["close"], "high": sc["high"], "low": sc["low"], "close": sc["close"],
                       "volume": volumes}, index=idx)
    df["open"] = np.clip(df["open"].shift(1).fillna(df["close"]), df["low"], df["high"])
    df.index.name = "datetime"
    series = {"coin": "BTC", **{k: CONTRACT[k] for k in PAYLOAD_CONTRACT_KEYS}, "intervals": intervals,
              "economic_evidence": True, "refusals": []}
    return df, series


def _run(df, series, **over):
    payload = payload_from_series(series, BAR)
    return _reg().apply_strategy(NAME, df, {**SEED, **over, "trade_flow_observations": payload})


def test_registration_is_futures_only_bidirectional_hidden_production_refused_with_a_valid_grid():
    reg = _reg()
    entry = reg.STRATEGY_REGISTRY[NAME]
    assert entry["production_refused"] is True and entry["edge_status"] == "no_edge"
    assert entry["edge_ref"] == "backtest/candidates/cumulative_volume_delta_divergence_1638/REPORT.md"
    assert NAME not in reg.list_strategies()
    spot = load_registry("spot")
    assert NAME not in spot.STRATEGY_REGISTRY
    registry = reg._registry
    assert registry.STRATEGIES[NAME]["short_entries"] is True
    assert registry.production_refused_strategies() == frozenset({NAME, "bitcoin_lead_lag", "macro_event_breakout",
                                                                  "volume_profile_reclaim", "perpetual_premium_reversion",
                                                                  "order_book_imbalance"})
    grid = generate_param_grid(DEFAULT_PARAM_RANGES[NAME])
    assert len(grid) == 216
    for params in grid:
        registry.validate_params(NAME, {**params, "expected_interval_ms": BAR})


def test_long_divergence_arms_at_confirmation_and_enters_once_on_a_strict_reclaim():
    df, series = _build(_scenario())
    out = _run(df, series)
    assert out["cvd_reason"].iloc[CONFIRM] == "armed_long"
    assert out["signal"].iloc[CONFIRM] == 0
    entries = np.flatnonzero(out["signal"].to_numpy() != 0)
    assert entries.tolist() == [CONFIRM + 2]
    row = out.iloc[CONFIRM + 2]
    assert row["cvd_reason"] == "entry_long" and row["cvd_side"] == "long"
    assert row["cvd_p1_ms"] == T0 + P1 * BAR and row["cvd_p2_ms"] == T0 + P2 * BAR
    assert row["cvd_confirm_ms"] == T0 + CONFIRM * BAR and row["cvd_deadline_ms"] == T0 + (CONFIRM + 3) * BAR
    assert row["cvd_p1_price"] == 95.0 and row["cvd_p2_price"] == 94.0 and row["cvd_reclaim"] == 101.0
    assert row["cvd_ratio"] == pytest.approx(0.2) and row["cvd_total_volume"] == pytest.approx(6.0)
    assert row["cvd_p2_delta"] - row["cvd_p1_delta"] == pytest.approx(1.2)
    assert row["cvd_setup_id"] == f"long:{T0 + CONFIRM * BAR}" and len(row["cvd_flow_sha256"]) == 64


def test_short_mirror_with_a_falling_delta():
    df, series = _build(_scenario(side="short", buy_frac="0.4"))
    out = _run(df, series)
    assert out["cvd_reason"].iloc[CONFIRM] == "armed_short"
    assert np.flatnonzero(out["signal"].to_numpy()).tolist() == [CONFIRM + 2]
    assert out["signal"].iloc[CONFIRM + 2] == -1 and out["cvd_ratio"].iloc[CONFIRM + 2] == pytest.approx(-0.2)


def test_price_only_baseline_enters_where_delta_disagrees():
    df, series = _build(_scenario(buy_frac="0.4"))
    held = _run(df, series)
    assert held["cvd_reason"].iloc[CONFIRM] == "delta_not_confirmed" and not held["signal"].any()
    base = _run(df, series, delta_confirmation=False)
    assert base["cvd_reason"].iloc[CONFIRM] == "armed_long" and base["signal"].iloc[CONFIRM + 2] == 1


def test_threshold_is_strict():
    for threshold, armed in ((0.2, False), (0.19, True)):
        with _case(f"{threshold}-{armed}"):
            df, series = _build(_scenario())
            out = _run(df, series, delta_threshold=threshold)
            assert (out["cvd_reason"].iloc[CONFIRM] == "armed_long") is armed


def test_equal_extrema_never_qualify():
    sc = _scenario()
    sc["low"][P2] = sc["low"][P1]
    df, series = _build(sc)
    assert not _run(df, series)["signal"].any()
    sc = _scenario()
    sc["low"][P2 - 1] = sc["low"][P2]
    df, series = _build(sc)
    assert not _run(df, series)["signal"].any()


def test_separation_bounds_and_session_crossing():
    df, series = _build(_scenario())
    assert not _run(df, series, min_separation=7)["signal"].any()
    assert not _run(df, series, max_separation=5, min_separation=2)["signal"].any()
    df, series = _build(_scenario(n=60))
    assert _run(df, series, session_hours=2)["signal"].iloc[CONFIRM + 2] == 1
    out = _run(df, series, session_hours=1)
    assert not out["signal"].any() and out["cvd_reason"].iloc[CONFIRM] == "no_setup"


def test_expiry_is_inclusive_and_clears_one_bar_later():
    expiry = SEED["setup_expiry"]
    df, series = _build(_scenario(entry_at=CONFIRM + expiry))
    assert _run(df, series)["signal"].iloc[CONFIRM + expiry] == 1
    df, series = _build(_scenario(entry_at=CONFIRM + expiry + 1))
    out = _run(df, series)
    assert not out["signal"].any() and out["cvd_reason"].iloc[CONFIRM + expiry + 1] == "setup_expired"


def test_confirmation_bar_cannot_enter_and_a_setup_is_consumed_once():
    sc = _scenario(entry_at=None)
    sc["high"][CONFIRM], sc["close"][CONFIRM] = 101.0, 100.9
    for i in (CONFIRM + 1, CONFIRM + 2):
        sc["high"][i], sc["close"][i] = 102.0, 101.5
    df, series = _build(sc)
    out = _run(df, series)
    assert out["signal"].iloc[CONFIRM] == 0
    assert np.flatnonzero(out["signal"].to_numpy()).tolist() == [CONFIRM + 1]


def test_zero_volume_and_missing_or_mismatched_flow_hold_without_inventing_volume():
    def zero(intervals, volumes):
        for iv in intervals:
            bar = (iv["start_ms"] - T0) // BAR
            if P1 < bar <= P2:
                iv.update(buy="0", sell="0", total="0", trades=0)
        for b in range(P1 + 1, P2 + 1):
            volumes[b] = 0.0
    df, series = _build(_scenario(), mutate=zero)
    assert _run(df, series)["cvd_reason"].iloc[CONFIRM] == "zero_flow_volume"

    def drop(intervals, volumes):
        intervals[:] = [iv for iv in intervals if iv["start_ms"] != T0 + 7 * BAR + 120_000]
    df, series = _build(_scenario(), mutate=drop)
    out = _run(df, series)
    assert out["cvd_reason"].iloc[CONFIRM] == "flow_unavailable" and not out["signal"].any()

    def mismatch(intervals, volumes):
        volumes[3] += 0.001
    df, series = _build(_scenario(), mutate=mismatch)
    assert _run(df, series)["cvd_reason"].iloc[CONFIRM] == "flow_unavailable"


def test_revocation_is_dated_and_clears_an_armed_setup():
    target = T0 + 6 * BAR

    def revoke(at):
        def mutate(intervals, volumes):
            for iv in intervals:
                if iv["start_ms"] == target:
                    iv["invalid_ms"], iv["reason"] = at, "late_event"
        return mutate
    before = T0 + CONFIRM * BAR
    df, series = _build(_scenario(), mutate=revoke(before))
    assert _run(df, series)["cvd_reason"].iloc[CONFIRM] == "flow_unavailable"
    after_entry = T0 + (CONFIRM + 4) * BAR
    df, series = _build(_scenario(), mutate=revoke(after_entry))
    assert _run(df, series)["signal"].iloc[CONFIRM + 2] == 1
    between = T0 + (CONFIRM + 1) * BAR + 30_000
    df, series = _build(_scenario(), mutate=revoke(between))
    out = _run(df, series)
    assert out["cvd_reason"].iloc[CONFIRM] == "armed_long"
    assert out["cvd_reason"].iloc[CONFIRM + 1] == "setup_cleared_flow" and not out["signal"].any()


def test_unverified_side_and_contract_changes_hold_every_entry():
    df, series = _build(_scenario())
    for key, value, reason in (("side_evidence", "unverified", "side_unverified"),
                               ("reorder_grace_ms", 1_000, "unsupported_trade_flow"),
                               ("source", "replica", "unsupported_trade_flow")):
        payload = dict(payload_from_series(series, BAR), **{key: value})
        out = _reg().apply_strategy(NAME, df, {**SEED, "trade_flow_observations": payload})
        assert set(out["cvd_reason"]) == {reason} and not out["signal"].any()
    out = _reg().apply_strategy(NAME, df, dict(SEED))
    assert set(out["cvd_reason"]) == {"trade_flow_unavailable"}


def test_mid_session_start_and_candle_gaps_hold_the_session():
    df, series = _build(_scenario())
    assert not _run(df.iloc[1:], series)["signal"].any()
    gapped = df.drop(df.index[20])
    out = _run(gapped, series)
    assert set(out["cvd_reason"].iloc[20:]) == {"incomplete_session_candles"}


def test_native_close_frames_decide_like_open_time_frames():
    df, series = _build(_scenario())
    native = df.copy()
    native.index = native.index + pd.Timedelta(milliseconds=BAR - 1)
    declared = declare_frame_cadence(native.copy(), "5m", TIMESTAMP_KIND_NATIVE_CLOSE)
    a = _run(df, series)
    for frame in (native, declared):
        b = _run(frame, series)
        assert a["signal"].tolist() == b["signal"].tolist() and a["cvd_reason"].tolist() == b["cvd_reason"].tolist()


def test_every_prefix_equals_the_deadline_specific_replay():
    def stagger(intervals, volumes):
        for iv in intervals:
            if (iv["start_ms"] // 60_000) % 9 == 0:
                iv["finalized_ms"] = iv["end_ms"] + 2 * BAR
            if (iv["start_ms"] // 60_000) % 31 == 0:
                iv["invalid_ms"], iv["reason"] = iv["end_ms"] + 3 * BAR, "late_event"
    for sc in (_scenario(), _scenario(side="short", buy_frac="0.4")):
        df, series = _build(sc, mutate=stagger)
        full = _run(df, series)
        cols = ["signal", "cvd_reason", "cvd_setup_id", "cvd_flow_sha256", "cvd_flow_available"]
        for i in range(len(df)):
            deadline = T0 + (i + 1) * BAR
            payload = payload_from_series(series, BAR, deadline)
            out = _reg().apply_strategy(NAME, df.iloc[:i + 1], {**SEED, "trade_flow_observations": payload})
            assert out[cols].iloc[-1].tolist() == full[cols].iloc[i].tolist(), i
            trunc = _run(df.iloc[:i + 1], series)
            assert trunc[cols].iloc[-1].tolist() == full[cols].iloc[i].tolist(), i


def test_a_live_payload_carries_the_closed_bar_decision_onto_the_forming_row():
    df, series = _build(_scenario())
    last_closed = CONFIRM + 2
    frame = df.iloc[:last_closed + 2]
    cutoff = T0 + (last_closed + 1) * BAR
    payload = payload_from_series(series, BAR, cutoff)
    out = _reg().apply_strategy(NAME, frame, {**SEED, "trade_flow_observations": payload})
    assert out["cvd_carried"].iloc[-1] and out["signal"].iloc[-1] == 1
    later = payload_from_series(series, BAR, cutoff + BAR)
    out = _reg().apply_strategy(NAME, frame, {**SEED, "trade_flow_observations": later})
    assert out["signal"].iloc[-1] == 0


def _composer(df, series, side="", ctx=None, flow=True):
    reg = _reg()
    params = dict(SEED, trade_flow_observations=payload_from_series(series, BAR)) if flow else dict(SEED)
    tp = {"tp_tiers": [{"profit_pct": 0.03, "close_fraction": 1.0}]}
    evaluation = evaluate_open_close(reg.apply_strategy, reg.get_strategy, df, NAME, NAME, ["tiered_tp_pct"], side,
                                     params=params, position_ctx=ctx, close_evaluate=close_evaluate,
                                     close_params_by_name={"tiered_tp_pct": tp})
    return finalize_decision(evaluation, side)


def test_composer_opens_both_directions_and_missing_flow_keeps_the_close_owner_executable():
    df, series = _build(_scenario())
    assert _composer(df.iloc[:CONFIRM + 3], series)["open_action"] == "long"
    sdf, sseries = _build(_scenario(side="short", buy_frac="0.4"))
    assert _composer(sdf.iloc[:CONFIRM + 3], sseries)["open_action"] == "short"
    mark = float(df["close"].iloc[CONFIRM + 2])

    def drop(intervals, volumes):
        intervals[:] = [iv for iv in intervals if iv["start_ms"] != T0 + 7 * BAR]
    _, gapped = _build(_scenario(), mutate=drop)
    for frame_series, flow in ((series, False), (gapped, True)):
        ctx = {"side": "long", "avg_cost": mark / 1.05, "entry_atr": 1.0, "current_quantity": 1.0, "initial_quantity": 1.0}
        decision = _composer(df.iloc[:CONFIRM + 3], frame_series, "long", ctx, flow=flow)
        assert decision["open_action"] == "none"
        assert decision["close_strategy"] == "tiered_tp_pct" and decision["close_fraction"] == 1.0
        assert decision["signal"] == -1
        flat = _composer(df.iloc[:CONFIRM + 3], frame_series, "", None, flow=flow)
        assert flat["signal"] == 0 and flat["open_action"] == "none"


def test_simulator_fills_next_open_and_closes_by_time_stop_or_the_atr_stop_while_flow_is_missing():
    blocks = []
    for j in range(6):
        sc = _scenario(n=48, side="long" if j % 2 == 0 else "short", buy_frac="0.6" if j % 2 == 0 else "0.4")
        blocks.append(sc)
    merged = {k: (np.concatenate([b[k] for b in blocks]) if k != "fracs" else sum((b[k] for b in blocks), []))
              for k in blocks[0]}
    df, series = _build(merged)
    sig = ensure_atr_indicator(_run(df, series))
    entries = np.flatnonzero(sig["signal"].to_numpy())
    assert len(entries) == 6
    bt = Backtester(initial_capital=1000.0, platform="hyperliquid",
                    open_strategy={"name": NAME, "params": dict(SEED)},
                    close_strategies=[{"name": "time_stop", "params": {"max_bars": 12}}],
                    stop_loss_atr_mult=1.0, direction="both", comparison_mode="approximate")
    res = bt.run(sig, strategy_name=NAME, symbol="BTC/USDT", timeframe="5m", save=False)
    trades = res["trades"]
    assert {t["side"] for t in trades} == {"long", "short"}
    for t in trades:
        pos = sig.index.get_loc(pd.Timestamp(t["entry_date"]))
        assert int(sig["signal"].iloc[pos - 1]) == (1 if t["side"] == "long" else -1)
        assert t["entry_price"] == pytest.approx(df["open"].iloc[pos], rel=0.01)
        assert t["exit_reason"].split(":")[0] in {"sl", "time_stop", "end_of_data"}

    def strip(intervals, volumes):
        intervals[:] = [iv for iv in intervals if iv["start_ms"] < T0 + 60 * BAR]
    df2, series2 = _build(merged, mutate=strip)
    sig2 = ensure_atr_indicator(_run(df2, series2))
    res2 = bt.run(sig2, strategy_name=NAME, symbol="BTC/USDT", timeframe="5m", save=False)
    assert res2["trades"] and all(t["exit_reason"].split(":")[0] in {"sl", "time_stop", "end_of_data"} for t in res2["trades"])
    assert len(res2["trades"]) < len(trades)


PRODUCTION_REFUSED = sorted(load_registry("futures")._registry.production_refused_strategies())


def test_production_admission_refuses_every_reference_and_keeps_existing_candidates():
    for refused in PRODUCTION_REFUSED:
        with _case(refused):
            _production_admission_refuses_every_reference_and_keeps_existing_candidates(refused)


def _production_admission_refuses_every_reference_and_keeps_existing_candidates(refused):
    NAME = refused
    reg = _reg()
    from close_registry_loader import get_strategy as get_close
    cases = [
        (NAME, None, None, GateMode(GATE_MODE_PAPER), Acknowledgement(False), "open"),
        (NAME, None, None, GateMode(GATE_MODE_LIVE), Acknowledgement(True), "open"),
        (NAME, None, None, GateMode(GATE_MODE_MISSING), Acknowledgement(True), "open"),
        ("breakout", NAME, None, GateMode(GATE_MODE_PAPER), Acknowledgement(False), "open"),
        (NAME, "breakout", None, GateMode(GATE_MODE_PAPER), Acknowledgement(False), "open"),
        ("breakout", None, [NAME], GateMode(GATE_MODE_PAPER), Acknowledgement(True), "close"),
    ]
    for positional, open_name, closes, mode, ack, role in cases:
        with pytest.raises(ValueError, match=f"{role} strategy '{NAME}' is production_refused"):
            admit_configured_strategies(positional, open_name, closes, mode, ack, reg.get_strategy, get_close)
    refs = admit_configured_strategies("mass_index_reversal", None, None, GateMode(GATE_MODE_PAPER),
                                       Acknowledgement(False), reg.get_strategy, get_close)
    assert [r.name for r in refs] == ["mass_index_reversal"]

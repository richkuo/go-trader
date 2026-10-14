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
from optimizer import DEFAULT_PARAM_RANGES, generate_param_grid
from premium_replay import CONTRACT, PAYLOAD_CONTRACT_KEYS, payload_from_series
from registry_loader import load_registry
from strategy_composition import (
    GATE_MODE_PAPER,
    Acknowledgement,
    GateMode,
    admit_configured_strategies,
    evaluate_open_close,
    finalize_decision,
)

NAME = "perpetual_premium_reversion"
BAR = 60_000
T0 = int(pd.Timestamp("2026-01-01T00:00:00Z").value // 1_000_000)
SEED = {"lookback_samples": 10, "entry_z": 2.0, "turn_span": 1, "variance_floor": 1e-10, "max_spread_bps": 5.0,
        "max_quote_age_ms": 5_000, "max_oracle_age_ms": 5_000, "max_skew_ms": 3_000, "max_abs_premium": 0.01,
        "max_mark_dislocation": 0.005, "expected_interval_ms": BAR}
ORACLE = 100.0


def _reg():
    return load_registry("futures")


def _dec(value: float) -> str:
    text = format(Decimal(repr(float(value))).quantize(Decimal("0.000001")).normalize(), "f")
    return text.rstrip("0").rstrip(".") if "." in text else text


def _premiums(n: int, extreme=None, sign=1.0, turn=0.5):
    base = [1e-4 * (k % 3) for k in range(n)]
    if extreme is not None:
        base[extreme] = sign * 8e-4
        if extreme + 1 < n:
            base[extreme + 1] = sign * 8e-4 * turn
    return base


def _sample(t, premium, session=1, quote_age=1_200, ctx_age=2_000, half_bps=0.5, mark_shift=0.0,
            status="sampled", invalid_ms=None):
    mid = ORACLE * (1.0 + premium)
    half = mid * half_bps / 10_000
    return {
        "t": t, "status": status, "finalized_ms": t + 1, "invalid_ms": invalid_ms,
        **({"reason": "malformed"} if status != "sampled" else {}),
        "quote": {"bid": _dec(mid - half), "bid_sz": "1.5", "bid_n": 3, "ask": _dec(mid + half), "ask_sz": "2",
                  "ask_n": 4, "event_ms": t - quote_age, "recv_ms": t - min(quote_age, 1_000), "session": session,
                  "seq": t // BAR},
        "context": {"oracle": _dec(ORACLE), "mark": _dec(mid * (1.0 + mark_shift)), "mid": None, "oracle_event_ms": None,
                    "recv_ms": t - ctx_age, "session": session, "seq": t // BAR},
    }


def _build(premiums, mutate=None):
    n = len(premiums)
    samples = [_sample(T0 + (i + 1) * BAR, p) for i, p in enumerate(premiums)]
    if mutate:
        mutate(samples)
    idx = pd.date_range(pd.Timestamp(T0, unit="ms"), periods=n, freq="1min")
    close = np.array([ORACLE * (1.0 + p) for p in premiums])
    df = pd.DataFrame({"open": close, "high": close * 1.0004, "low": close * 0.9996, "close": close,
                       "volume": np.full(n, 10.0)}, index=idx)
    df["open"] = np.clip(df["open"].shift(1).fillna(df["close"]), df["low"], df["high"])
    df.index.name = "datetime"
    series = {"coin": "BTC", **{k: CONTRACT[k] for k in PAYLOAD_CONTRACT_KEYS}, "samples": samples,
              "economic_evidence": True, "refusals": []}
    return df, series


def _run(df, series, cutoff=None, **over):
    payload = payload_from_series(series, BAR, cutoff, None if cutoff is None else 30 * BAR)
    return _reg().apply_strategy(NAME, df, {**SEED, **over, "premium_observations": payload})


def _row(out, i):
    return out["signal"].iloc[i], out["ppr_reason"].iloc[i]


def test_registration_is_futures_only_bidirectional_hidden_production_refused_with_a_valid_grid():
    reg = _reg()
    entry = reg.STRATEGY_REGISTRY[NAME]
    assert entry["production_refused"] is True and entry["edge_status"] == "no_edge"
    assert entry["edge_ref"] == "backtest/candidates/perpetual_premium_reversion_1639/REPORT.md"
    assert NAME not in reg.list_strategies()
    assert NAME not in load_registry("spot").STRATEGY_REGISTRY
    registry = reg._registry
    assert registry.STRATEGIES[NAME]["short_entries"] is True
    assert NAME in registry.production_refused_strategies()
    grid = generate_param_grid(DEFAULT_PARAM_RANGES[NAME])
    assert len(grid) == 12
    for params in grid:
        registry.validate_params(NAME, params)


def test_parameter_validation_refuses_an_unestablishable_oracle_source_age_and_bad_bounds():
    registry = _reg()._registry
    with pytest.raises(ValueError, match="no source time"):
        registry.validate_params(NAME, {"max_oracle_source_age_ms": 1000})
    for bad in ({"turn_span": 10, "lookback_samples": 10}, {"expected_interval_ms": 90_000}, {"entry_z": 0},
                {"variance_floor": 0}, {"lookback_samples": 5}, {"unknown": 1}):
        with pytest.raises(ValueError):
            registry.validate_params(NAME, bad)


def test_short_enters_after_a_positive_extreme_turns_and_long_mirrors():
    df, series = _build(_premiums(30, extreme=20))
    out = _run(df, series)
    entries = np.flatnonzero(out["signal"].to_numpy())
    assert entries.tolist() == [21]
    sig, why = _row(out, 21)
    assert sig == -1 and why == "entry_short"
    t = T0 + 22 * BAR
    history = [1e-4 * (k % 3) for k in range(11, 20)] + [8e-4]
    mean = sum(history) / 10
    std = (sum((x - mean) ** 2 for x in history) / 10) ** 0.5
    assert out["ppr_sample_ms"].iloc[21] == t
    assert out["ppr_mean"].iloc[21] == pytest.approx(mean, rel=1e-6)
    assert out["ppr_std"].iloc[21] == pytest.approx(std, rel=1e-6)
    assert out["ppr_z_extreme"].iloc[21] == pytest.approx((8e-4 - mean) / std, rel=1e-6)
    assert out["ppr_premium"].iloc[21] == pytest.approx(4e-4, rel=1e-4)
    assert out["ppr_side"].iloc[21] == "short" and len(out["ppr_window_sha256"].iloc[21]) == 64
    for col in ("ppr_bid", "ppr_ask", "ppr_oracle", "ppr_quote_event_ms", "ppr_quote_recv_ms", "ppr_context_recv_ms"):
        assert np.isfinite(out[col].iloc[21])
    ldf, lseries = _build(_premiums(30, extreme=20, sign=-1.0))
    lout = _run(ldf, lseries)
    assert np.flatnonzero(lout["signal"].to_numpy()).tolist() == [21]
    assert _row(lout, 21) == (1, "entry_long")


def test_comparisons_are_strict_at_the_frozen_boundaries():
    df, series = _build(_premiums(30, extreme=20, turn=1.0))
    out = _run(df, series)
    assert _row(out, 21) == (0, "no_turn")
    df, series = _build(_premiums(30, extreme=20))
    z = _run(df, series)["ppr_z_extreme"].iloc[21]
    assert _row(_run(df, series, entry_z=z), 21) == (0, "no_extreme")
    assert _row(_run(df, series, entry_z=z * 0.999), 21)[0] == -1
    crossed = _premiums(30, extreme=20, turn=-0.1)
    df, series = _build(crossed)
    assert _row(_run(df, series), 21) == (0, "wrong_premium_sign")
    flat = [2e-4] * 30
    df, series = _build(flat)
    assert set(_run(df, series)["ppr_reason"].iloc[10:]) == {"variance_floor"}


def test_invalid_or_incomplete_inputs_hold_with_a_reason():
    cases = {
        "sample_missing": lambda s: s.pop(15),
        "sample_not_sampled": lambda s: s[15].update(status="invalid", reason="malformed"),
        "stale_quote": lambda s: s[15]["quote"].update(event_ms=s[15]["t"] - 5_001),
        "stale_oracle": lambda s: s[15]["context"].update(recv_ms=s[15]["t"] - 5_001),
        "component_skew": lambda s: s[15]["context"].update(recv_ms=s[15]["quote"]["recv_ms"] - 3_001),
        "session_change": lambda s: [x["quote"].update(session=2) or x["context"].update(session=2) for x in s[16:]],
        "wide_spread": lambda s: s[15]["quote"].update(bid="99.9", ask="100.1"),
        "premium_dislocation": lambda s: s[15]["quote"].update(bid="101.5", ask="101.51"),
        "mark_dislocation": lambda s: s[15]["context"].update(mark="99"),
        "malformed_sample": lambda s: s[15]["quote"].update(bid="1e5"),
    }
    for reason, mutate in cases.items():
        df, series = _build(_premiums(30, extreme=20), mutate=lambda s, m=mutate: m(s))
        out = _run(df, series)
        assert _row(out, 21) == (0, reason), reason
        assert _row(out, 27) == (0, "no_extreme"), reason
    boundary = {
        "stale_quote": lambda s: s[15]["quote"].update(event_ms=s[15]["t"] - 5_000),
        "stale_oracle": lambda s: (s[15]["context"].update(recv_ms=s[15]["t"] - 5_000),
                                   s[15]["quote"].update(recv_ms=s[15]["t"] - 2_500, event_ms=s[15]["t"] - 2_600)),
        "component_skew": lambda s: s[15]["context"].update(recv_ms=s[15]["quote"]["recv_ms"] - 3_000),
    }
    for reason, mutate in boundary.items():
        df, series = _build(_premiums(30, extreme=20), mutate=lambda s, m=mutate: m(s))
        assert _row(_run(df, series), 21)[0] == -1, reason


def test_a_later_revocation_applies_only_from_its_detection_time():
    def revoke_after(s):
        s[15]["invalid_ms"] = T0 + 23 * BAR
        s[15]["invalid_reason"] = "conflicting_duplicate"
    df, series = _build(_premiums(30, extreme=20), mutate=revoke_after)
    out = _run(df, series)
    assert _row(out, 21)[0] == -1
    assert _row(out, 22) == (0, "sample_invalidated")

    def revoke_before(s):
        s[15]["invalid_ms"] = T0 + 22 * BAR
        s[15]["invalid_reason"] = "conflicting_duplicate"
    df, series = _build(_premiums(30, extreme=20), mutate=revoke_before)
    assert _row(_run(df, series), 21) == (0, "sample_invalidated")


def test_contract_and_availability_failures_hold_every_entry():
    df, series = _build(_premiums(30, extreme=20))
    for key, value in (("contract", "other"), ("oracle_time_basis", "receipt"), ("sample_ms", 30_000), ("kind", "open_interest")):
        payload = {**payload_from_series(series, BAR), key: value}
        out = _reg().apply_strategy(NAME, df, {**SEED, "premium_observations": payload})
        assert not out["signal"].any() and set(out["ppr_reason"]) == {"unsupported_premium"}
    unavailable = payload_from_series({**series, "economic_evidence": False, "refusals": ["damaged"]}, BAR)
    out = _reg().apply_strategy(NAME, df, {**SEED, "premium_observations": unavailable})
    assert set(out["ppr_reason"]) == {"premium_unavailable"}
    out = _reg().apply_strategy(NAME, df, dict(SEED))
    assert set(out["ppr_reason"]) == {"premium_unavailable"}

    def oracle_time(s):
        s[3]["context"]["oracle_event_ms"] = s[3]["context"]["recv_ms"]
    df2, series2 = _build(_premiums(30, extreme=20), mutate=oracle_time)
    assert _row(_run(df2, series2), 13) == (0, "malformed_sample")


def test_future_samples_and_bars_never_change_an_earlier_decision():
    df, series = _build(_premiums(40, extreme=20))
    full = _run(df, series)
    for cut in (21, 25, 33):
        truncated = {**series, "samples": [s for s in series["samples"] if s["t"] <= T0 + cut * BAR]}
        out = _run(df.iloc[:cut], truncated)
        assert out["signal"].tolist() == full["signal"].iloc[:cut].tolist()
        assert out["ppr_reason"].tolist() == full["ppr_reason"].iloc[:cut].tolist()
        perturbed = [dict(s) for s in series["samples"]]
        for s in perturbed:
            if s["t"] > T0 + cut * BAR:
                s.update(status="invalid", reason="malformed")
        out = _run(df, {**series, "samples": perturbed})
        assert out["signal"].iloc[:cut].tolist() == full["signal"].iloc[:cut].tolist()


def test_a_live_payload_carries_the_closed_bar_decision_onto_the_forming_row():
    df, series = _build(_premiums(30, extreme=20))
    frame = df.iloc[:23]
    cutoff = T0 + 22 * BAR
    out = _run(frame, series, cutoff=cutoff)
    assert out["ppr_carried"].iloc[-1] and out["signal"].iloc[-1] == -1
    later = _run(frame, series, cutoff=cutoff + BAR)
    assert later["signal"].iloc[-1] == 0


def _composer(df, series, side="", ctx=None, premium=True):
    reg = _reg()
    params = dict(SEED, premium_observations=payload_from_series(series, BAR)) if premium else dict(SEED)
    evaluation = evaluate_open_close(reg.apply_strategy, reg.get_strategy, df, NAME, NAME, ["time_stop"], side,
                                     params=params, position_ctx=ctx, close_evaluate=close_evaluate,
                                     close_params_by_name={"time_stop": {"max_bars": 5}})
    return finalize_decision(evaluation, side)


def test_composer_opens_both_directions_and_failed_premium_inputs_keep_the_close_owner_executable():
    df, series = _build(_premiums(30, extreme=20))
    assert _composer(df.iloc[:22], series)["open_action"] == "short"
    ldf, lseries = _build(_premiums(30, extreme=20, sign=-1.0))
    assert _composer(ldf.iloc[:22], lseries)["open_action"] == "long"
    ctx = {"side": "short", "avg_cost": float(df["close"].iloc[21]), "entry_atr": 0.05, "current_quantity": 1.0,
           "initial_quantity": 1.0, "bars_held": 6}
    _, broken = _build(_premiums(30, extreme=20), mutate=lambda s: [x.update(status="invalid", reason="malformed") for x in s])
    managed = {}
    for label, frame_series, premium in (("intact", series, True), ("unavailable", series, False), ("invalid", broken, True)):
        decision = _composer(df.iloc[:29], frame_series, "short", ctx, premium=premium)
        assert decision["open_action"] == "none", label
        managed[label] = (decision["close_strategy"], decision["close_fraction"], decision["signal"])
    assert managed["unavailable"] == managed["intact"] == managed["invalid"]
    assert managed["intact"][0] == "time_stop" and managed["intact"][1] == 1.0
    flat = _composer(df.iloc[:29], broken, "", None)
    assert flat["signal"] == 0 and flat["open_action"] == "none"


def test_simulator_fills_next_open_and_closes_by_time_stop_or_the_atr_stop():
    premiums = []
    for j in range(6):
        premiums += _premiums(40, extreme=25, sign=1.0 if j % 2 == 0 else -1.0)
    df, series = _build(premiums)
    sig = ensure_atr_indicator(_run(df, series))
    entries = np.flatnonzero(sig["signal"].to_numpy())
    assert len(entries) == 6
    bt = Backtester(initial_capital=1000.0, platform="hyperliquid",
                    open_strategy={"name": NAME, "params": dict(SEED)},
                    close_strategies=[{"name": "time_stop", "params": {"max_bars": 10}}],
                    stop_loss_atr_mult=1.0, direction="both", comparison_mode="approximate")
    res = bt.run(sig, strategy_name=NAME, symbol="BTC/USDT", timeframe="1m", save=False)
    trades = res["trades"]
    assert {t["side"] for t in trades} == {"long", "short"}
    for t in trades:
        pos = sig.index.get_loc(pd.Timestamp(t["entry_date"]))
        assert int(sig["signal"].iloc[pos - 1]) == (1 if t["side"] == "long" else -1)
        assert t["entry_price"] == pytest.approx(df["open"].iloc[pos], rel=0.01)
        assert t["exit_reason"].split(":")[0] in {"sl", "time_stop", "end_of_data"}


def test_production_admission_refuses_the_candidate_and_keeps_existing_candidates():
    reg = _reg()
    from close_registry_loader import get_strategy as get_close
    with pytest.raises(ValueError, match=f"open strategy '{NAME}' is production_refused"):
        admit_configured_strategies(NAME, None, None, GateMode(GATE_MODE_PAPER), Acknowledgement(True),
                                    reg.get_strategy, get_close)
    refs = admit_configured_strategies("mass_index_reversal", None, None, GateMode(GATE_MODE_PAPER),
                                       Acknowledgement(False), reg.get_strategy, get_close)
    assert [r.name for r in refs] == ["mass_index_reversal"]

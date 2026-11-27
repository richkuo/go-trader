import os
import sys
from decimal import ROUND_HALF_EVEN, Decimal

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
from premium_replay import CONTRACT, PAYLOAD_CONTRACT_KEYS, payload_from_series
from registry_loader import load_registry
from strategy_composition import (
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
    text = format(Decimal(repr(float(value))).quantize(Decimal("0.000001"), rounding=ROUND_HALF_EVEN).normalize(), "f")
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

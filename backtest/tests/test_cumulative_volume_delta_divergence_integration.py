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
from registry_loader import load_registry
from strategy_composition import (
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


def _threshold_is_strict():
    for threshold, armed in ((0.2, False), (0.19, True)):
        with _case(f"{threshold}-{armed}"):
            df, series = _build(_scenario())
            out = _run(df, series, delta_threshold=threshold)
            assert (out["cvd_reason"].iloc[CONFIRM] == "armed_long") is armed


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

import importlib.util
import json
import math
import os
import subprocess
import sys

import numpy as np
import pandas as pd
import pytest

REPO = os.path.abspath(os.path.join(os.path.dirname(os.path.abspath(__file__)), ".."))
CHECK_HL = os.path.join(REPO, "shared_scripts", "check_hyperliquid.py")
CREDENTIAL_PREFIXES = ("HYPERLIQUID", "OKX", "ROBINHOOD", "TOPSTEP", "PROJECTX")
NAME = "williams_r_reversal"
H = 3_600_000
T0 = 1_699_999_200_000
TP_REFS = {"open": {"name": NAME, "params": {}},
           "closes": [{"name": "tiered_tp_atr",
                       "params": {"tp_tiers": [{"atr_multiple": 2.0, "close_fraction": 1.0}]}}]}


def _env():
    env = {k: v for k, v in os.environ.items() if not k.startswith(CREDENTIAL_PREFIXES)}
    env["GO_TRADER_HL_OHLCV_CACHE"] = "0"
    return env


def _run(argv, stdin=None):
    return subprocess.run(
        [sys.executable, CHECK_HL] + argv,
        input=None if stdin is None else json.dumps(stdin),
        capture_output=True, text=True, cwd=REPO, env=_env(), timeout=240,
    )


def _payload(proc):
    return json.loads(proc.stdout.strip().splitlines()[-1])


def _strategies():
    sys.path.insert(0, os.path.join(REPO, "shared_tools"))
    path = os.path.join(REPO, "shared_strategies", "open", "futures", "strategies.py")
    spec = importlib.util.spec_from_file_location("_williams_r_integration_futures", path)
    mod = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(mod)
    return mod


def _zigzag(n, start=100.0):
    out = []
    price = start
    for i in range(n):
        price += 0.4 if i % 2 == 0 else -0.35
        out.append((price - 0.1, price + 0.5, price - 0.5, price))
    return out


def _with_long_cross(n):
    bars = _zigzag(n - 6)
    px = bars[-1][3]
    for _ in range(5):
        px -= 1.5
        bars.append((px + 1.5, px + 1.5, px, px))
    bars.append((px, px + 3.0, px, px + 2.9))
    return bars


def _with_short_cross(n):
    bars = _zigzag(n - 6)
    px = bars[-1][3]
    for _ in range(5):
        px += 1.5
        bars.append((px - 1.5, px, px - 1.5, px))
    bars.append((px, px, px - 3.0, px - 2.9))
    return bars


def _rows(bars, interval_ms=H):
    return [[T0 + (i + 1) * interval_ms - 1, o, h, low, c, 1000.0 + i] for i, (o, h, low, c) in enumerate(bars)]


def _frame(bars, *, timing=True, interval_ms=H):
    rows = _rows(bars, interval_ms)
    frame = {"rows": rows, "required": len(rows), "bars": len(rows), "coverage_short": False,
             "first_open_ms": T0, "last_open_ms": T0 + (len(bars) - 1) * interval_ms,
             "last_close_ms": rows[-1][0], "last_recv_at_ms": rows[-1][0], "source": "ws",
             "ready": True, "forming_bar_included": True}
    if timing:
        frame["timing"] = {"rule": "hyperliquid_native_close", "interval_ms": interval_ms,
                           "bars": [[T0 + i * interval_ms, T0 + (i + 1) * interval_ms - 1, True]
                                    for i in range(len(bars))]}
    return frame


def _market(bars, *, cutoff_ms=None, timing=True, mid=0.0):
    market = {"version": 1, "snapshot_id": "test/1", "generation": 1,
              "sealed_at_ms": cutoff_ms or T0, "frames": {"BTC|1h": _frame(bars, timing=timing)},
              "feed_complete": True}
    if cutoff_ms is not None:
        market["decision_cutoff_ms"] = cutoff_ms
    if mid:
        market["mids"] = {"BTC": {"px": mid, "recv_at_ms": cutoff_ms or T0, "source": "ws",
                                  "age_ms": 0, "stale": False, "confirmed": True}}
    return market


def _solo(bars, extra=(), *, cutoff_ms=None, timing=True, mid=0.0):
    return _run([NAME, "BTC", "1h", "--market-stdin", *extra],
                {"v": 2, "market": _market(bars, cutoff_ms=cutoff_ms, timing=timing, mid=mid)})


def _ok(proc):
    assert proc.returncode == 0, proc.stderr[-3000:] + proc.stdout[-2000:]
    out = _payload(proc)
    assert not out.get("error")
    return out


def _frame_from_bars(bars):
    rows = _rows(bars)
    df = pd.DataFrame(rows, columns=["timestamp", "open", "high", "low", "close", "volume"])
    df["datetime"] = pd.to_datetime(df["timestamp"], unit="ms", utc=True)
    return df.set_index("datetime").sort_index()


@pytest.mark.parametrize("bars,signal,action,code", [
    (_with_long_cross(120), 1, "long", 1),
    (_with_short_cross(120), -1, "short", -1),
    (_zigzag(120), 0, "none", 0),
])
def test_explicit_paper_check_emits_the_registry_decision_and_explanation(bars, signal, action, code):
    out = _ok(_solo(bars, ["--mode=paper", "--strategy-refs", json.dumps(TP_REFS)]))
    expected = _strategies().apply_strategy(NAME, _frame_from_bars(bars), None).iloc[-1]
    assert out["signal"] == signal
    assert out["open_action"] == action
    assert out["close_strategies"] == ["tiered_tp_atr"]
    ind = out["indicators"]
    assert ind["wr_reason_code"] == code == int(expected["wr_reason_code"])
    assert ind["wr"] == round(float(expected["wr"]), 6)
    assert ind["wr_prev"] == round(float(expected["wr_prev"]), 6)
    assert ind["wr_lookback"] == 14 and ind["wr_oversold"] == -80.0 and ind["wr_overbought"] == -20.0
    assert ind["wr_valid"] == 1.0
    assert ind["wr_eval_ts_ms"] == float(T0 + len(bars) * H - 1)


@pytest.mark.parametrize("last_close,want_wr,want_signal", [
    (92.0, -80.0, 0),
    (92.5, -75.0, 1),
    (91.0, -90.0, 0),
])
def test_oversold_equality_on_the_current_bar_is_not_a_crossing(last_close, want_wr, want_signal):
    bars = _zigzag(60) + [(96.0, 100.0, 95.0, 96.0), (95.0, 100.0, 90.0, 90.0), (90.0, 95.0, 91.0, last_close)]
    res = _strategies().apply_strategy(NAME, _frame_from_bars(bars), {"lookback": 2}).iloc[-1]
    assert res["wr_prev"] == -100.0
    assert res["wr"] == want_wr
    out = _ok(_solo(bars, ["--mode=paper", "--params", json.dumps({"lookback": 2})]))
    assert out["signal"] == want_signal == int(res["signal"])
    assert out["indicators"]["wr"] == want_wr


@pytest.mark.parametrize("last_close,want_wr,want_signal", [
    (98.0, -20.0, 0),
    (97.5, -25.0, -1),
    (99.0, -10.0, 0),
])
def test_overbought_equality_on_the_current_bar_is_not_a_crossing(last_close, want_wr, want_signal):
    bars = _zigzag(60) + [(94.0, 95.0, 90.0, 94.0), (95.0, 100.0, 90.0, 100.0), (99.0, 99.0, 95.0, last_close)]
    res = _strategies().apply_strategy(NAME, _frame_from_bars(bars), {"lookback": 2}).iloc[-1]
    assert res["wr_prev"] == 0.0
    assert res["wr"] == want_wr
    out = _ok(_solo(bars, ["--mode=paper", "--params", json.dumps({"lookback": 2})]))
    assert out["signal"] == want_signal == int(res["signal"])


@pytest.mark.parametrize("extra,reason", [
    ([], "missing mode"),
    (["--mode=live"], "explicit live mode"),
    (["--mode=paper", "--mode=paper"], "invalid mode input"),
    (["--mode=Paper"], "invalid mode input"),
])
def test_missing_live_or_malformed_mode_is_refused(extra, reason):
    proc = _solo(_zigzag(80), extra)
    assert proc.returncode == 1
    err = _payload(proc)["error"]
    assert f"open strategy '{NAME}' is edge_status=no_edge" in err
    assert reason in err


def test_existing_acknowledgement_contract_still_admits_live_evaluation():
    out = _ok(_solo(_zigzag(80), ["--mode=live", "--allow-no-edge"]))
    assert out["strategy"] == NAME


def test_no_edge_close_fallback_reference_is_gated():
    refs = json.dumps({"open": {"name": "breakout"}, "closes": [{"name": NAME}]})
    proc = _run(["breakout", "BTC", "1h", "--mode=live", "--market-stdin", "--strategy-refs", refs],
                {"v": 2, "market": _market(_zigzag(80))})
    assert proc.returncode == 1
    err = _payload(proc)["error"]
    assert f"close strategy '{NAME}' is edge_status=no_edge" in err and "explicit live mode" in err
    _ok(_run(["breakout", "BTC", "1h", "--mode=paper", "--market-stdin", "--strategy-refs", refs],
             {"v": 2, "market": _market(_zigzag(80))}))


@pytest.mark.parametrize("params", [{"lookback": 2.5}, {"lookback": "14"}, {"lookback": True},
                                    {"oversold": -10.0, "overbought": -20.0}, {"overbought": 1.0}])
def test_invalid_parameters_fail_preflight_before_any_candle_fetch(params):
    proc = _run([NAME, "BTC", "1h", "--mode=paper", "--params", json.dumps(params)])
    assert proc.returncode == 1
    err = _payload(proc)["error"]
    assert f"open strategy '{NAME}' has invalid parameters" in err
    assert "Fetching" not in proc.stderr


def test_batch_preflight_isolates_invalid_slots_and_keeps_valid_slots():
    bars = _with_long_cross(120)
    bad_close = {"open": {"name": "breakout"}, "closes": [{"name": NAME, "params": {"lookback": 0}}]}
    slots = [
        {"id": "bad", "strategy": NAME, "mode": "paper", "mode_args": ["--mode=paper"],
         "strategy_refs": {"open": {"name": NAME, "params": {"lookback": 7.5}}}},
        {"id": "bad-close", "strategy": "breakout", "mode": "paper", "mode_args": ["--mode=paper"],
         "strategy_refs": bad_close},
        {"id": "good", "strategy": NAME, "mode": "paper", "mode_args": ["--mode=paper"],
         "strategy_refs": TP_REFS},
    ]
    proc = _run(["--batch-check", "--symbol=BTC", "--timeframe=1h", "--market-stdin"],
                {"v": 2, "slots": slots, "market": _market(bars)})
    assert proc.returncode == 1
    results = {r["id"]: r for r in _payload(proc)["results"]}
    assert f"open strategy '{NAME}' has invalid parameters" in results["bad"]["error"]
    assert f"close strategy '{NAME}' has invalid parameters" in results["bad-close"]["error"]
    assert not results["good"].get("error")
    solo = _ok(_solo(bars, ["--mode=paper", "--strategy-refs", json.dumps(TP_REFS)]))
    for key in ("signal", "open_action", "close_fraction", "indicators"):
        assert results["good"][key] == solo[key]


@pytest.mark.parametrize("mutate,reason,code", [
    (lambda b: b[:-3] + [(b[-3][0], b[-3][1], b[-3][2], float("nan"))] + b[-2:], "nonfinite_input", 11),
    (lambda b: b[:-3] + [(b[-3][0], b[-3][2] - 1.0, b[-3][1], b[-3][3])] + b[-2:], "inconsistent_hlc", 13),
    (lambda b: b[:-2] + [(0.0, 0.0, 0.0, 0.0)] + b[-1:], "nonpositive_price", 12),
    (lambda b: b[:-20] + [(100.0, 100.0, 100.0, 100.0)] * 20, "zero_range_window", 16),
])
def test_invalid_entry_input_holds_and_keeps_take_profit_protection(mutate, reason, code):
    bars = mutate(_with_long_cross(120))
    df = _frame_from_bars(bars)
    res = _strategies().apply_strategy(NAME, df, None)
    assert res["wr_reason"].iloc[-1] == reason
    assert int(res["signal"].iloc[-1]) == 0
    flat = _ok(_solo(bars, ["--mode=paper", "--strategy-refs", json.dumps(TP_REFS)]))
    assert flat["signal"] == 0 and flat["open_action"] == "none"
    assert flat["indicators"]["wr_reason_code"] == code
    assert "wr" not in flat["indicators"]
    held = _ok(_solo(bars, ["--mode=paper", "--strategy-refs", json.dumps(TP_REFS), "--position-side", "long",
                            "--position-avg-cost=50", "--position-qty=1", "--position-initial-qty=1",
                            "--position-entry-atr=2"], mid=100.0))
    assert held["open_action"] == "none"
    assert held["close_fraction"] == 1.0
    assert held["close_strategy"] == "tiered_tp_atr"
    assert held["signal"] == -1


def test_open_position_blocks_a_new_entry_signal():
    bars = _with_short_cross(120)
    held = _ok(_solo(bars, ["--mode=paper", "--strategy-refs", json.dumps(TP_REFS), "--position-side", "long",
                            "--position-avg-cost=100", "--position-qty=1", "--position-initial-qty=1",
                            "--position-entry-atr=500"]))
    assert held["open_action"] == "short"
    assert held["close_fraction"] == 0.0
    assert held["signal"] == 0


def test_closed_bar_decision_ignores_the_forming_tail_and_matches_next_bar_execution():
    closed = _with_long_cross(130)
    forming = closed + [(closed[-1][3], closed[-1][3] + 0.2, closed[-1][3] - 40.0, closed[-1][3] - 39.0)]
    cutoff = T0 + (len(forming) - 1) * H + H // 2
    disabled = _ok(_solo(forming, ["--mode=paper"], cutoff_ms=cutoff))
    enabled = _ok(_solo(forming, ["--mode=paper", "--closed-bar-decisions"], cutoff_ms=cutoff))
    reference = _ok(_solo(closed, ["--mode=paper", "--closed-bar-decisions"], cutoff_ms=cutoff))
    meta = enabled["closed_bar_decision"]
    assert meta["held"] is False
    assert meta["forming_rows_dropped"] == 1
    assert meta["cutoff_ms"] == cutoff
    assert meta["decision_boundary_ms"] == T0 + len(closed) * H
    assert enabled["signal"] == 1
    assert disabled["signal"] == 0
    assert enabled["indicators"] == reference["indicators"]
    assert enabled["closed_bar_decision"]["input_sha256"] == reference["closed_bar_decision"]["input_sha256"]

    sys.path.insert(0, os.path.join(REPO, "backtest"))
    from backtester import Backtester
    df = _strategies().apply_strategy(NAME, _frame_from_bars(forming + [forming[-1]] * 2), None)
    bt = Backtester(initial_capital=1000.0, commission_pct=0.0, slippage_pct=0.0,
                    platform="hyperliquid", strategy_type="perps")
    trade = bt.run(df, strategy_name=NAME, symbol="BTC", timeframe="1h", save=False)["trades"][0]
    assert pd.Timestamp(trade["entry_date"]) == df.index[len(closed)]


@pytest.mark.parametrize("offset,dropped", [(-1, 1), (0, 0), (1, 0)])
def test_cutoff_at_the_bar_boundary_selects_the_closed_bar(offset, dropped):
    bars = _with_long_cross(130)
    boundary = T0 + len(bars) * H
    out = _ok(_solo(bars, ["--mode=paper", "--closed-bar-decisions"], cutoff_ms=boundary + offset))
    assert out["closed_bar_decision"]["forming_rows_dropped"] == dropped
    assert out["signal"] == (1 if dropped == 0 else 0)


def test_missing_timing_holds_the_decision_and_keeps_protection():
    bars = _with_long_cross(130)
    cutoff = T0 + len(bars) * H + 5
    refs = ["--strategy-refs", json.dumps(TP_REFS)]
    flat = _ok(_solo(bars, ["--mode=paper", "--closed-bar-decisions", *refs], cutoff_ms=cutoff, timing=False))
    assert flat["closed_bar_decision"]["held"] is True
    assert flat["signal"] == 0 and flat["indicators"] == {}
    held = _ok(_solo(bars, ["--mode=paper", "--closed-bar-decisions", *refs, "--position-side", "long",
                            "--position-avg-cost=50", "--position-qty=1", "--position-initial-qty=1",
                            "--position-entry-atr=2"], cutoff_ms=cutoff, timing=False, mid=100.0))
    assert held["closed_bar_decision"]["held"] is True
    assert held["close_fraction"] == 1.0 and held["signal"] == -1


def test_full_series_prefix_and_bounded_window_agree_at_every_allowed_lookback():
    mod = _strategies()
    rng = np.random.RandomState(1650)
    n = 212
    close = 100 + np.cumsum(rng.randn(n))
    span = np.abs(rng.randn(n)) + 0.2
    bars = [(c, c + s, c - s, c + (s * 0.5 if i % 3 else -s * 0.5)) for i, (c, s) in enumerate(zip(close, span))]
    bars = [(o, max(h, c), min(low, c), c) for o, h, low, c in bars]
    bars[60] = (bars[60][0], bars[60][2] - 1.0, bars[60][1], bars[60][3])
    bars[130] = (bars[130][0], float("nan"), bars[130][2], bars[130][3])
    df = _frame_from_bars(bars)
    cols = ["signal", "wr_reason", "wr", "wr_prev", "wr_highest_high", "wr_lowest_low", "wr_valid"]

    def same(a, b):
        for col in cols:
            x, y = a[col], b[col]
            if isinstance(x, float) and math.isnan(x):
                assert isinstance(y, float) and math.isnan(y), col
            else:
                assert x == y, col

    for lookback in range(2, 101):
        params = {"lookback": lookback}
        full = mod.apply_strategy(NAME, df, params)
        assert (full["wr_reason"].iloc[:min(lookback, 60)] == "insufficient_history").all()
        for i in range(n):
            prefix = mod.apply_strategy(NAME, df.iloc[:i + 1], params).iloc[-1]
            same(full.iloc[i], prefix)
            if i >= 199:
                bounded = mod.apply_strategy(NAME, df.iloc[i - 199:i + 1], params).iloc[-1]
                same(full.iloc[i], bounded)
        assert (full["wr_reason"].iloc[60:61] == "inconsistent_hlc").all()
        recovered = full["wr_valid"].iloc[61 + lookback:130]
        assert recovered.all() if len(recovered) else True

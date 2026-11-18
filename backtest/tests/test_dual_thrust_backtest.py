from shared_tools.conftest import with_regime_readiness
import json
import math
import os
import subprocess
import sys

import numpy as np
import pandas as pd
import pytest

_REPO = os.path.abspath(os.path.join(os.path.dirname(__file__), "..", ".."))
if os.path.join(_REPO, "shared_tools") not in sys.path:
    sys.path.insert(0, os.path.join(_REPO, "shared_tools"))

from atr import ensure_atr_indicator
from backtester import Backtester
from close_registry_loader import evaluate as close_evaluate
from closed_bar import declare_frame_cadence
from parity_diff import _ENGINE_SLIPPAGE_PCT, ParityConfig
from registry_loader import load_registry
from strategy_composition import evaluate_open_close, finalize_decision

NAME = "dual_thrust"
H = 3_600_000
DAY = 86_400_000
T0 = 1_767_225_600_000
P = {"lookback_sessions": 2, "k_long": 0.5, "k_short": 0.5, "session_start_hour": 0, "expected_interval_ms": H}
TP = {"name": "tiered_tp_pct", "params": {"tp_tiers": [{"profit_pct": 0.03, "close_fraction": 1.0}]}}
COLS = ["signal", "dt_reason", "dt_valid", "dt_range", "dt_anchor", "dt_upper", "dt_lower", "dt_hh", "dt_lc",
        "dt_hc", "dt_ll", "dt_long_consumed", "dt_short_consumed", "dt_eval_ts_ms", "dt_session_open_ms"]
CHECK_HL = os.path.join(_REPO, "shared_scripts", "check_hyperliquid.py")
CREDENTIAL_PREFIXES = ("HYPERLIQUID", "OKX", "ROBINHOOD", "TOPSTEP", "PROJECTX")


def _reg():
    return load_registry("futures")


def _apply(df, **kw):
    return _reg().apply_strategy(NAME, df, dict(P, **kw))


def _frame(bars, freq="1h", start="2026-01-01", kind="open"):
    df = pd.DataFrame(bars, columns=["open", "high", "low", "close"], dtype=float)
    df["volume"] = 1.0
    df.index = pd.date_range(start, periods=len(df), freq={"30m": "30min"}.get(freq, freq))
    return declare_frame_cadence(df, freq, kind)


def _bar(o, c, pad=0.1):
    return (o, max(o, c) + pad, min(o, c) - pad, c)


def _base(per=24):
    day1 = [(100.0, 100.5, 99.5, 100.0)] * per
    day1[2] = (100.0, 105.0, 99.5, 100.0)
    day1[-1] = (100.0, 101.0, 99.9, 101.0)
    day2 = [(100.0, 100.5, 99.5, 100.0)] * per
    day2[1] = (100.0, 100.4, 96.0, 100.0)
    day2[-1] = (100.0, 100.2, 99.0, 99.0)
    return day1 + day2


def _session(closes, per=24, anchor=100.0, fill=100.0):
    rows, o = [], anchor
    for c in closes:
        rows.append(_bar(o, c))
        o = c
    while len(rows) < per:
        rows.append(_bar(o, fill))
        o = fill
    return rows


def _walk(n, seed=1642, freq="1h", start="2026-01-01", step=1.2):
    rng = np.random.RandomState(seed)
    c = np.round(100 + 4 * np.sin(np.arange(n) / 17.0) + np.cumsum(rng.randn(n) * step), 2)
    o = np.concatenate([[c[0]], c[:-1]])
    hi = np.maximum(o, c) + np.round(np.abs(rng.randn(n)) * 0.6, 2)
    lo = np.minimum(o, c) - np.round(np.abs(rng.randn(n)) * 0.6, 2)
    return _frame(list(zip(o, hi, lo, c)), freq=freq, start=start)


def _reference(df, n, kl, ks, hour, interval):
    per = DAY // interval
    ms = np.array([t.value // 1_000_000 for t in df.index])
    sess = (ms - hour * H) // DAY
    daily = {}
    for s in np.unique(sess):
        rows = df[sess == s]
        if len(rows) == per:
            daily[s] = (rows["high"].max(), rows["low"].min(), rows["close"].iloc[-1])
    sig = np.zeros(len(df), dtype=int)
    vals = {}
    fired = set()
    for i in range(len(df)):
        s = sess[i]
        prior = [s - j for j in range(1, n + 1)]
        first = int(np.flatnonzero(sess == s)[0])
        if not all(p in daily for p in prior) or (ms[first] - hour * H) % DAY:
            continue
        hh = max(daily[p][0] for p in prior)
        ll = min(daily[p][1] for p in prior)
        hc = max(daily[p][2] for p in prior)
        lc = min(daily[p][2] for p in prior)
        rng = max(hh - lc, hc - ll)
        anchor = df["open"].iloc[first]
        vals[i] = (hh, lc, hc, ll, rng, anchor)
        close = df["close"].iloc[i]
        if close > anchor + kl * rng and (s, 1) not in fired:
            sig[i] = 1
            fired.add((s, 1))
        elif close < anchor - ks * rng and (s, -1) not in fired:
            sig[i] = -1
            fired.add((s, -1))
    return sig, vals


def _same(a, b):
    for col in COLS:
        x, y = a[col], b[col]
        if isinstance(x, float) and isinstance(y, float) and math.isnan(x) and math.isnan(y):
            continue
        assert x == y, (col, x, y)


def _days(n_days, per=24):
    bars = _base(per)
    while len(bars) < n_days * per:
        bars += _session([101.0, 99.0, 103.5, 96.5], per=per)
    return bars


def test_timezone_column_and_native_close_inputs_resolve_the_same_opening_times():
    df = _walk(24 * 12, seed=5)
    base = _apply(df)
    tz = declare_frame_cadence(df.tz_localize("UTC").tz_convert("America/New_York"), "1h", "open")
    col = df.reset_index(drop=True)
    col["timestamp"] = df.index.as_unit("ms").asi8
    col = declare_frame_cadence(col, "1h", "open")
    native_idx = df.index + pd.Timedelta(milliseconds=H - 1)
    native = df.copy()
    native.index = native_idx
    native = declare_frame_cadence(native, "1h", "native_close_inclusive")
    undeclared_native = df.copy()
    undeclared_native.index = native_idx.tz_localize("UTC")
    undeclared_native.attrs = {}
    undeclared_open = df.tz_localize("UTC")
    undeclared_open.attrs = {}
    for other in (tz, col, native, undeclared_native, undeclared_open):
        got = _apply(other)
        for c in ("signal", "dt_valid", "dt_eval_ts_ms", "dt_upper", "dt_lower", "dt_reason"):
            assert np.array_equal(base[c].to_numpy(), got[c].to_numpy(),
                                  equal_nan=base[c].dtype.kind == "f"), c
    assert (base["signal"] != 0).any()


def _bt(**kw):
    base = dict(initial_capital=1000.0, platform="hyperliquid", open_strategy={"name": NAME, "params": P},
                close_strategies=[TP], stop_loss_atr_mult=1.0, direction="both")
    base.update(kw)
    return Backtester(**base)


def _run_bt(df, params=None, **kw):
    sig = ensure_atr_indicator(_apply(df, **(params or {})))
    return sig, _bt(**kw).run(with_regime_readiness(sig), strategy_name=NAME, symbol="BTC/USDT", timeframe="1h", save=False)


def _long_entry_fills_at_the_next_bar_open_and_the_explicit_close_owns_the_exit():
    df = _frame(_base() + _session([101.0, 102.0, 103.5, 104.2, 105.0, 106.0, 107.5]))
    sig, res = _run_bt(df)
    assert int(np.flatnonzero(sig["signal"].to_numpy() == 1)[0]) == 50
    t = res["trades"][0]
    assert t["side"] == "long" and t["entry_date"] == str(df.index[51])
    assert t["entry_price"] == pytest.approx(df["open"].iloc[51] * (1 + _ENGINE_SLIPPAGE_PCT))
    assert "tp" in t["exit_reason"]


def _short_entry_fills_at_the_next_bar_open():
    df = _frame(_base() + _session([99.0, 98.0, 96.5, 95.5, 94.5, 93.5, 92.0]))
    sig, res = _run_bt(df)
    t = res["trades"][0]
    assert t["side"] == "short" and t["entry_date"] == str(df.index[51])
    assert t["entry_price"] == pytest.approx(df["open"].iloc[51] * (1 - _ENGINE_SLIPPAGE_PCT))


def _opposite_and_later_signals_never_reverse_or_add_while_open():
    tomorrow = _session([110.0, 80.0])
    df = _frame(_base() + _session([103.5, 100.0, 96.5, 101.0]) + tomorrow)
    sig, res = _run_bt(df, stop_loss_atr_mult=None, stop_loss_pct=60.0,
                       close_strategies=[{"name": "tiered_tp_pct",
                                          "params": {"tp_tiers": [{"profit_pct": 0.5, "close_fraction": 1.0}]}}])
    s = sig["signal"].to_numpy()
    assert (s == 1).sum() == 2 and (s == -1).sum() == 2
    trades = res["trades"]
    assert len(trades) == 1 and trades[0]["side"] == "long"
    assert trades[0]["entry_date"] == str(df.index[49])


SIMULATOR_FILLS = [
    ("long_fills_next_open_close_owns_exit", _long_entry_fills_at_the_next_bar_open_and_the_explicit_close_owns_the_exit),
    ("short_fills_next_open", _short_entry_fills_at_the_next_bar_open),
    ("no_reversal_or_add_while_open", _opposite_and_later_signals_never_reverse_or_add_while_open),
]


def test_simulator_fills_each_side_at_the_next_open_without_reversal_or_add(subtests):
    for label, run in SIMULATOR_FILLS:
        with subtests.test(case=label):
            run()


def test_an_event_blocked_by_the_regime_gate_never_reappears_later_in_its_session():
    today = _session([103.5, 104.0, 104.5, 105.0, 104.5], fill=104.0)
    tomorrow = _session([105.0, 106.0])
    df = _frame(_base() + today + tomorrow)
    sig = ensure_atr_indicator(_apply(df))
    sig["regime"] = np.where((np.arange(len(sig)) >= 47) & (np.arange(len(sig)) <= 50), "ranging", "trending")
    assert list(np.flatnonzero(sig["signal"].to_numpy() == 1)) == [48, 72]
    assert (sig["dt_reason"].iloc[49:72] == "long_consumed").all()
    wide = [{"name": "tiered_tp_pct", "params": {"tp_tiers": [{"profit_pct": 0.5, "close_fraction": 1.0}]}}]
    gated = _bt(close_strategies=wide, stop_loss_atr_mult=None, stop_loss_pct=60.0, regime_enabled=True,
                allowed_regimes=["trending"]).run(with_regime_readiness(sig), strategy_name=NAME, symbol="BTC/USDT", timeframe="1h",
                                                  save=False)
    assert [t["entry_date"] for t in gated["trades"]] == [str(df.index[73])]
    open_gate = _bt(close_strategies=wide, stop_loss_atr_mult=None, stop_loss_pct=60.0).run(
        sig.drop(columns=["regime"]), strategy_name=NAME, symbol="BTC/USDT", timeframe="1h", save=False)
    assert [t["entry_date"] for t in open_gate["trades"]] == [str(df.index[49])]


def test_composer_suppresses_entries_while_open_consumes_the_event_and_keeps_closes_on_invalid_data():
    fut = _reg()
    df = _frame(_base() + _session([101.0, 103.5, 104.0, 104.5]))
    pos = {"side": "long", "avg_cost": 100.0, "current_quantity": 1.0, "initial_quantity": 1.0}
    kwargs = dict(close_evaluate=close_evaluate, close_params_by_name={"tiered_tp_pct": TP["params"]})
    held = evaluate_open_close(fut.apply_strategy, fut.get_strategy, df.iloc[:50], NAME, None, ["tiered_tp_pct"],
                               "long", dict(P), pos, market_ctx={"mark_price": 101.0}, **kwargs)
    assert held.open_signal == 1
    decided = finalize_decision(held, "long")
    assert decided["signal"] == 0 and decided["close_fraction"] == 0.0
    later = evaluate_open_close(fut.apply_strategy, fut.get_strategy, df.iloc[:51], NAME, None,
                                ["tiered_tp_pct"], "", dict(P), {}, market_ctx={"mark_price": 104.0}, **kwargs)
    assert later.open_signal == 0 and later.open_result_df["dt_reason"].iloc[-1] == "long_consumed"
    assert finalize_decision(later, "")["signal"] == 0

    gapped = df.iloc[:51].drop(df.index[49])
    out = evaluate_open_close(fut.apply_strategy, fut.get_strategy, gapped, NAME, None, ["tiered_tp_pct"], "long",
                              dict(P), pos, market_ctx={"mark_price": 104.0}, **kwargs)
    assert out.open_signal == 0 and out.open_result_df["dt_reason"].iloc[-1] == "cadence_gap"
    closed = finalize_decision(out, "long")
    assert closed["close_fraction"] == 1.0 and closed["close_strategy"] == "tiered_tp_pct" and closed["signal"] == -1

    flat = finalize_decision(evaluate_open_close(fut.apply_strategy, fut.get_strategy, df.iloc[:50], NAME, None,
                                                 ["tiered_tp_pct"], "", dict(P), {},
                                                 market_ctx={"mark_price": 103.5}, **kwargs), "")
    assert flat["open_action"] == "long" and flat["signal"] == 1


def _parity_cfg(freq, **kw):
    interval = H if freq == "1h" else 4 * H
    base = dict(strategy_name=NAME, params={"lookback_sessions": 2, "k_long": 0.25, "k_short": 0.25,
                                            "session_start_hour": 0, "expected_interval_ms": interval},
                registry="futures", platform="hyperliquid", symbol="BTC", timeframe=freq,
                close_refs=[TP], direction="both")
    base.update(kw)
    return ParityConfig(**base)


def _env():
    env = {k: v for k, v in os.environ.items() if not k.startswith(CREDENTIAL_PREFIXES)}
    env["GO_TRADER_HL_OHLCV_CACHE"] = "0"
    return env


def _run(argv, stdin=None):
    return subprocess.run([sys.executable, CHECK_HL] + argv, input=None if stdin is None else json.dumps(stdin),
                          capture_output=True, text=True, cwd=_REPO, env=_env(), timeout=240)


def _payload(proc):
    return json.loads(proc.stdout.strip().splitlines()[-1])


def _hl_bars(df):
    out = []
    for ts, r in df.iterrows():
        t = int(ts.value // 1_000_000)
        out.append({"t": t, "T": t + H - 1, "o": float(r["open"]), "h": float(r["high"]), "l": float(r["low"]),
                    "c": float(r["close"]), "v": 1000.0})
    return out


def _hl_frame(bars, timing=True):
    rows = [[b["T"], b["o"], b["h"], b["l"], b["c"], b["v"]] for b in bars]
    frame = {"rows": rows, "required": len(rows), "bars": len(rows), "coverage_short": False,
             "first_open_ms": bars[0]["t"], "last_open_ms": bars[-1]["t"], "last_close_ms": bars[-1]["T"],
             "last_recv_at_ms": bars[-1]["t"], "source": "ws", "ready": True, "forming_bar_included": True}
    if timing:
        frame["timing"] = {"rule": "hyperliquid_native_close", "interval_ms": H,
                           "bars": [[b["t"], b["T"], True] for b in bars]}
    return frame


def _market(bars, cutoff_ms=None, mid=None, timing=True):
    market = {"version": 4, "snapshot_id": "3600s/1", "generation": 1, "sealed_at_ms": cutoff_ms or bars[-1]["T"],
              "frames": {"BTC|1h": _hl_frame(bars, timing=timing)}, "feed_complete": True}
    if cutoff_ms is not None:
        market["decision_cutoff_ms"] = cutoff_ms
    px = mid if mid is not None else bars[-1]["c"]
    market["mids"] = {"BTC": {"px": px, "recv_at_ms": bars[-1]["T"], "source": "ws", "age_ms": 0, "stale": False,
                              "confirmed": True}}
    return market


def _crossing_bars():
    df = _frame(_days(8) + _session([100.5, 101.0, 101.5, 103.5], per=4))
    return _hl_bars(df)


def _hl_check(bars, *, closed, extra=(), mid=None, cutoff=None):
    argv = [NAME, "BTC", "1h", "--mode=paper", "--market-stdin", "--ohlcv-limit", "200", *extra]
    if closed:
        argv.append("--closed-bar-decisions")
    cutoff = cutoff if cutoff is not None else bars[-1]["t"] + H // 2
    refs = json.dumps({"open": {"name": NAME, "params": dict(P)}, "closes": [TP]})
    proc = _run(argv + ["--strategy-refs", refs], {"v": 2, "market": _market(bars, cutoff, mid)})
    assert proc.returncode == 0, proc.stderr[-3000:] + proc.stdout[-2000:]
    return _payload(proc)


def _closed_bar_decisions_drop_the_forming_bar_and_report_full_precision_inputs():
    bars = _crossing_bars()
    enabled = _hl_check(bars, closed=True)
    disabled = _hl_check(bars, closed=False)
    assert enabled["closed_bar_decision"]["held"] is False
    assert enabled["closed_bar_decision"]["forming_rows_dropped"] == 1
    assert disabled["signal"] == 1 and enabled["signal"] == 0
    ind = enabled["indicators"]
    assert ind["dt_valid"] == 1.0 and ind["dt_eval_ts_ms"] == float(bars[-2]["t"])
    assert ind["dt_close"] == bars[-2]["c"] and ind["dt_bar_in_session"] == 2.0
    assert enabled["indicator_labels"]["dt_reason"] == "inside_range"
    dind = disabled["indicators"]
    assert disabled["indicator_labels"]["dt_reason"] == "entry_long"
    assert dind["dt_anchor"] == 100.0 and dind["dt_close"] == 103.5
    assert dind["dt_range"] == max(dind["dt_hh"] - dind["dt_lc"], dind["dt_hc"] - dind["dt_ll"])
    assert dind["dt_upper"] == dind["dt_anchor"] + 0.5 * dind["dt_range"]
    calm = _hl_check(bars[:-1], closed=True, cutoff=bars[-1]["t"] + H // 2)
    assert calm["indicators"] == ind


def _closed_bar_decisions_emit_the_session_long_from_the_closed_bar_after_a_forming_miss():
    bars = _crossing_bars()
    crossing = bars[-1]
    forming_inside = dict(crossing, h=crossing["o"] + 0.1, l=crossing["o"] - 0.1, c=crossing["o"])
    early = bars[:-1] + [forming_inside]
    early_default = _hl_check(early, closed=False)
    assert early_default["signal"] == 0 and early_default["indicator_labels"]["dt_reason"] == "inside_range"
    assert _hl_check(early, closed=True)["signal"] == 0
    nxt = {"t": crossing["t"] + H, "T": crossing["T"] + H, "o": crossing["c"], "h": crossing["c"] + 0.6,
           "l": crossing["c"] - 0.1, "c": crossing["c"] + 0.5, "v": 1000.0}
    late = bars + [nxt]
    late_closed = _hl_check(late, closed=True)
    assert late_closed["closed_bar_decision"]["forming_rows_dropped"] == 1
    assert late_closed["signal"] == 1 and late_closed["indicator_labels"]["dt_reason"] == "entry_long"
    assert late_closed["indicators"]["dt_eval_ts_ms"] == float(crossing["t"])
    late_default = _hl_check(late, closed=False)
    assert late_default["signal"] == 0 and late_default["indicator_labels"]["dt_reason"] == "long_consumed"


def test_closed_bar_decisions_select_the_last_closed_bar_and_report_its_inputs(subtests):
    with subtests.test(case="drop_forming_bar_and_report_inputs"):
        _closed_bar_decisions_drop_the_forming_bar_and_report_full_precision_inputs()
    with subtests.test(case="session_long_from_closed_bar_after_forming_miss"):
        _closed_bar_decisions_emit_the_session_long_from_the_closed_bar_after_a_forming_miss()


def test_batch_slots_match_individual_checks():
    bars = _crossing_bars()[-200:]
    cutoff = bars[-1]["t"] + H // 2
    market = _market(bars, cutoff)
    refs = {"open": {"name": NAME, "params": dict(P)}, "closes": [TP]}
    slots = [
        {"id": "on", "strategy": NAME, "mode": "paper", "mode_args": ["--mode=paper"], "closed_bar_decisions": True,
         "strategy_refs": refs},
        {"id": "off", "strategy": NAME, "mode": "paper", "mode_args": ["--mode=paper"], "strategy_refs": refs},
    ]
    proc = _run(["--batch-check", "--symbol=BTC", "--timeframe=1h", "--ohlcv-limit", "200", "--atr-method=simple",
                 "--market-stdin"], {"v": 2, "slots": slots, "market": market})
    assert proc.returncode == 0, proc.stderr[-3000:]
    results = {r["id"]: r for r in json.loads(proc.stdout)["results"]}
    for slot_id, closed in (("on", True), ("off", False)):
        argv = [NAME, "BTC", "1h", "--mode=paper", "--market-stdin", "--ohlcv-limit", "200", "--strategy-refs",
                json.dumps(refs)]
        if closed:
            argv.append("--closed-bar-decisions")
        single = _run(argv, {"v": 2, "market": market})
        assert single.returncode == 0, single.stderr[-3000:]
        solo = _payload(single)
        for key in ("signal", "open_action", "close_fraction", "indicators", "indicator_labels"):
            assert results[slot_id].get(key) == solo.get(key), (slot_id, key)
    assert results["off"]["signal"] == 1 and results["on"]["signal"] == 0


RUN_BACKTEST = os.path.join(_REPO, "backtest", "run_backtest.py")


def test_run_backtest_binds_the_interval_from_a_4h_timeframe(tmp_path):
    import storage
    db = str(tmp_path / "ohlcv.db")
    storage.init_db(db)
    df = _walk(6 * 120, freq="4h", start="2025-01-01", seed=41, step=2.5)
    rows = df.reset_index(drop=True)
    rows.insert(0, "timestamp", (df.index.astype("int64") // 10**6).astype("int64"))
    storage.store_ohlcv(rows, "binanceus", "BTC/USDT", "4h", db_path=db)
    env = dict(os.environ, GO_TRADER_OHLCV_CACHE_DB=db)
    cmd = [sys.executable, RUN_BACKTEST, "--mode", "single", "--strategy", NAME, "--registry", "futures",
           "--platform", "hyperliquid", "--symbol", "BTC/USDT", "--since", "2025-01-01", "--timeframe", "4h",
           "--funding", "off", "--direction", "both", "--close-strategy", json.dumps(TP),
           "--stop-loss-atr-mult", "1.0"]
    proc = subprocess.run(cmd, cwd=_REPO, env=env, capture_output=True, text=True, timeout=600)
    assert proc.returncode == 0, proc.stderr + proc.stdout
    params = next(l for l in proc.stdout.splitlines() if l.strip().startswith("Params:"))
    assert "'expected_interval_ms': 14400000" in params
    trades = [l for l in proc.stdout.splitlines() if "Total Trades:" in l]
    assert trades and int(trades[0].split(":")[1]) > 0

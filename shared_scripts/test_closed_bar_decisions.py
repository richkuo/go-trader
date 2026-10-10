import json
import os
import subprocess
import sys
import textwrap

import pandas as pd
import pytest


_REPO_ROOT = os.path.abspath(os.path.join(os.path.dirname(os.path.abspath(__file__)), ".."))
CHECK_HL = os.path.join(_REPO_ROOT, "shared_scripts", "check_hyperliquid.py")
CHECK_SPOT = os.path.join(_REPO_ROOT, "shared_scripts", "check_strategy.py")
CHECK_OKX = os.path.join(_REPO_ROOT, "shared_scripts", "check_okx.py")
CHECK_RH = os.path.join(_REPO_ROOT, "shared_scripts", "check_robinhood.py")
CHECK_TS = os.path.join(_REPO_ROOT, "shared_scripts", "check_topstep.py")

H = 3_600_000
T0 = 1_699_999_200_000
SPEC = '{"default":{"classifier":"adx","period":14,"adx_threshold":20}}'


def _bars(n, *, spike_at=None, spike_size=12.0, forming_spike=0.0):
    out = []
    price = 100.0
    for i in range(n):
        price = price + (0.4 if i % 2 == 0 else -0.35)
        o, h, low, c = price - 0.1, price + 0.5, price - 0.5, price
        if spike_at is not None and i == spike_at:
            h, c = price + spike_size + 0.5, price + spike_size
        if forming_spike and i == n - 1:
            h, c = price + forming_spike + 0.5, price + forming_spike
        out.append({"t": T0 + i * H, "T": T0 + (i + 1) * H - 1,
                    "o": o, "h": h, "l": low, "c": c, "v": 1000.0 + i})
    return out


def _hl_rows(bars):
    return [[b["T"], b["o"], b["h"], b["l"], b["c"], b["v"]] for b in bars]


def _hl_frame(bars, *, timing=True, interval_ms=H):
    rows = _hl_rows(bars)
    frame = {
        "rows": rows, "required": len(rows), "bars": len(rows), "coverage_short": False,
        "first_open_ms": bars[0]["t"], "last_open_ms": bars[-1]["t"], "last_close_ms": bars[-1]["T"],
        "last_recv_at_ms": bars[-1]["t"], "source": "ws", "ready": True, "forming_bar_included": True,
    }
    if timing:
        frame["timing"] = {"rule": "hyperliquid_native_close", "interval_ms": interval_ms,
                           "bars": [[b["t"], b["T"], True] for b in bars]}
    return frame


def _market(frames, *, cutoff_ms=None, mid=0.0):
    market = {"version": 1, "snapshot_id": "test/1", "generation": 1,
              "sealed_at_ms": cutoff_ms or T0, "frames": frames, "feed_complete": True}
    if cutoff_ms is not None:
        market["decision_cutoff_ms"] = cutoff_ms
    if mid:
        market["mids"] = {"BTC": {"px": mid, "recv_at_ms": cutoff_ms or T0, "source": "ws",
                                  "age_ms": 0, "stale": False, "confirmed": True}}
    return market


def _run(script, argv, stdin_payload=None, env_extra=None):
    env = dict(os.environ)
    env["HYPERLIQUID_SECRET_KEY"] = ""
    env["HYPERLIQUID_ACCOUNT_ADDRESS"] = ""
    env["OKX_API_KEY"] = ""
    env["GO_TRADER_HL_OHLCV_CACHE"] = "0"
    if env_extra:
        env.update(env_extra)
    proc = subprocess.run(
        [sys.executable, script] + argv,
        input=json.dumps(stdin_payload) if stdin_payload is not None else "",
        capture_output=True, text=True, cwd=_REPO_ROOT, env=env, timeout=240,
    )
    return proc


def _hl_check(name, bars, cutoff_ms, *, closed, extra=(), timing=True, frames=None, mid=0.0):
    argv = [name, "BTC", "1h", "--mode=paper", "--market-stdin", *extra]
    if closed:
        argv.append("--closed-bar-decisions")
    frames = frames or {"BTC|1h": _hl_frame(bars, timing=timing)}
    proc = _run(CHECK_HL, argv, {"v": 2, "market": _market(frames, cutoff_ms=cutoff_ms, mid=mid)})
    assert proc.returncode == 0, proc.stderr + proc.stdout
    return json.loads(proc.stdout)


def _strip(out):
    return {k: v for k, v in out.items() if k not in ("timestamp", "id")}


def _strategies(kind="futures"):
    sys.path.insert(0, os.path.join(_REPO_ROOT, "shared_tools"))
    import importlib.util
    path = os.path.join(_REPO_ROOT, "shared_strategies", "open", kind, "strategies.py")
    spec = importlib.util.spec_from_file_location(f"_closed_bar_test_{kind}_strategies", path)
    mod = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(mod)
    return mod


def _frame_from_rows(rows):
    df = pd.DataFrame(rows, columns=["timestamp", "open", "high", "low", "close", "volume"])
    df["datetime"] = pd.to_datetime(df["timestamp"], unit="ms", utc=True)
    return df.set_index("datetime").sort_index()


def _backtest_first_trade(name, rows, atr_method="simple"):
    sys.path.insert(0, os.path.join(_REPO_ROOT, "backtest"))
    from backtester import Backtester
    df = _strategies().apply_strategy(name, _frame_from_rows(rows), None)
    bt = Backtester(initial_capital=1000.0, commission_pct=0.0, slippage_pct=0.0,
                    platform="hyperliquid", strategy_type="perps", atr_method=atr_method)
    result = bt.run(df, strategy_name=name, symbol="BTC", timeframe="1h", save=False)
    return result["trades"][0] if result["trades"] else None


def test_forming_tail_decision_uses_the_last_closed_bar_and_matches_the_backtester():
    bars = _bars(120, spike_at=118, spike_size=12.0)
    cutoff = bars[-1]["t"] + H // 2
    disabled = _hl_check("breakout", bars, cutoff, closed=False)
    enabled = _hl_check("breakout", bars, cutoff, closed=True)

    meta = enabled["closed_bar_decision"]
    assert meta["held"] is False
    assert meta["decision_boundary_ms"] == bars[-2]["t"] + H
    assert meta["bar_open_ms"] == bars[-2]["t"]
    assert meta["forming_rows_dropped"] == 1
    assert meta["cutoff_ms"] == cutoff
    assert "closed_bar_decision" not in disabled

    closed_rows = _hl_rows(bars[:-1])
    closed_df = _strategies().apply_strategy("breakout", _frame_from_rows(closed_rows), None)
    assert enabled["signal"] == int(closed_df["signal"].iloc[-1]) == 1
    assert disabled["signal"] == 0
    assert enabled["indicators"]["atr"] == round(float(closed_df["atr"].iloc[-1]), 6)
    assert enabled["indicators"]["atr"] != disabled["indicators"]["atr"]
    assert enabled["price"] == disabled["price"] == bars[-1]["c"]

    trade = _backtest_first_trade("breakout", _hl_rows(bars))
    assert trade is not None
    assert pd.Timestamp(trade["entry_date"]) == pd.Timestamp(bars[-1]["T"], unit="ms", tz="UTC")
    assert trade["entry_atr"] == pytest.approx(enabled["indicators"]["atr"], abs=1e-6)


def test_forming_spike_changes_the_disabled_signal_but_not_the_closed_decision():
    bars = _bars(120, forming_spike=15.0)
    cutoff = bars[-1]["t"] + H // 3
    disabled = _hl_check("breakout", bars, cutoff, closed=False)
    enabled = _hl_check("breakout", bars, cutoff, closed=True)
    calm = _hl_check("breakout", bars[:-1], cutoff, closed=True)
    assert disabled["signal"] == 1
    assert enabled["signal"] == 0
    assert enabled["indicators"] == calm["indicators"]
    assert enabled["closed_bar_decision"]["input_sha256"] == calm["closed_bar_decision"]["input_sha256"]


def test_final_row_already_closed_is_kept():
    bars = _bars(120, spike_at=119)
    cutoff = bars[-1]["t"] + H + 5_000
    disabled = _hl_check("breakout", bars, cutoff, closed=False)
    enabled = _hl_check("breakout", bars, cutoff, closed=True)
    meta = enabled["closed_bar_decision"]
    assert meta["forming_rows_dropped"] == 0
    assert meta["decision_boundary_ms"] == bars[-1]["t"] + H
    assert enabled["signal"] == disabled["signal"] == 1
    assert enabled["indicators"] == disabled["indicators"]


@pytest.mark.parametrize("offset,dropped", [(-1, 1), (0, 0), (1, 0)])
def test_cutoff_at_the_bar_boundary(offset, dropped):
    bars = _bars(120)
    boundary = bars[-1]["t"] + H
    enabled = _hl_check("breakout", bars, boundary + offset, closed=True)
    assert enabled["closed_bar_decision"]["forming_rows_dropped"] == dropped
    expected = boundary if dropped == 0 else bars[-1]["t"]
    assert enabled["closed_bar_decision"]["decision_boundary_ms"] == expected


@pytest.mark.parametrize("method", ["simple", "wilder"])
def test_exported_atr_follows_the_atr_method_on_the_closed_frame(method):
    sys.path.insert(0, os.path.join(_REPO_ROOT, "backtest"))
    import backtester
    bars = _bars(120, forming_spike=20.0)
    cutoff = bars[-1]["t"] + H // 2
    enabled = _hl_check("momentum_pro", bars, cutoff, closed=True, extra=[f"--atr-method={method}"])
    disabled = _hl_check("momentum_pro", bars, cutoff, closed=False, extra=[f"--atr-method={method}"])
    closed = _frame_from_rows(_hl_rows(bars[:-1]))
    expected = round(float(backtester.standard_atr(closed, method=method).iloc[-1]), 6)
    assert enabled["indicators"]["atr"] == expected
    assert disabled["indicators"]["atr"] != expected


def test_composed_strategy_decides_on_the_closed_bar_and_protection_reads_the_current_mark():
    bars = _bars(120, spike_at=118)
    cutoff = bars[-1]["t"] + H // 2
    refs = json.dumps({"open": {"name": "breakout", "params": {}}, "closes": [{"name": "tiered_tp_pct", "params": {}}]})
    flat = ["--strategy-refs", refs]
    enabled = _hl_check("breakout", bars, cutoff, closed=True, extra=flat)
    assert enabled["open_action"] == "long"
    assert enabled["signal"] == 1

    held_long = ["--strategy-refs", refs, "--position-side", "long", "--position-avg-cost=50",
                 "--position-qty=1", "--position-initial-qty=1"]
    on = _hl_check("breakout", bars, cutoff, closed=True, extra=held_long, mid=bars[-1]["c"])
    off = _hl_check("breakout", bars, cutoff, closed=False, extra=held_long, mid=bars[-1]["c"])
    assert on["close_fraction"] == off["close_fraction"] == 1.0
    assert on["close_strategy"] == off["close_strategy"] == "tiered_tp_pct"
    assert on["price"] == off["price"] == bars[-1]["c"]


def test_missing_timing_holds_the_decision_and_keeps_protection():
    bars = _bars(120, spike_at=118)
    cutoff = bars[-1]["t"] + H // 2
    refs = json.dumps({"open": {"name": "breakout", "params": {}}, "closes": [{"name": "tiered_tp_pct", "params": {}}]})
    held_long = ["--strategy-refs", refs, "--position-side", "long", "--position-avg-cost=50",
                 "--position-qty=1", "--position-initial-qty=1"]
    out = _hl_check("breakout", bars, cutoff, closed=True, extra=held_long, timing=False)
    meta = out["closed_bar_decision"]
    assert meta["held"] is True
    assert "timing" in meta["hold_reason"]
    assert out["indicators"] == {}
    assert out["open_action"] == "none"
    assert out["close_fraction"] == 1.0
    assert out["signal"] == -1

    flat = _hl_check("breakout", bars, cutoff, closed=True, timing=False)
    assert flat["signal"] == 0
    assert flat["closed_bar_decision"]["held"] is True


@pytest.mark.parametrize("case", ["no_cutoff", "contradictory", "duplicate", "short_history"])
def test_unverifiable_or_short_history_holds(case):
    bars = _bars(120, spike_at=118)
    cutoff = bars[-1]["t"] + H // 2
    frames = None
    if case == "no_cutoff":
        cutoff = None
    elif case == "contradictory":
        bars[-3] = dict(bars[-3], T=bars[-3]["T"] + 60_000)
    elif case == "duplicate":
        bars[-3] = dict(bars[-4])
    elif case == "short_history":
        bars = bars[-30:]
    out = _hl_check("breakout", bars, cutoff, closed=True, frames=frames)
    assert out["signal"] == 0
    assert out["closed_bar_decision"]["held"] is True
    assert out["indicators"] == {}


def test_mixed_batch_matches_individual_checks():
    bars = _bars(240, spike_at=238)
    cutoff = bars[-1]["t"] + H // 2
    market = _market({"BTC|1h": _hl_frame(bars)}, cutoff_ms=cutoff, mid=bars[-1]["c"])
    slots = [
        {"id": "on", "strategy": "breakout", "mode": "paper", "mode_args": ["--mode=paper"], "closed_bar_decisions": True},
        {"id": "off", "strategy": "breakout", "mode": "paper", "mode_args": ["--mode=paper"]},
    ]
    proc = _run(CHECK_HL, ["--batch-check", "--symbol=BTC", "--timeframe=1h", "--ohlcv-limit", "200",
                           "--atr-method=simple", "--market-stdin"], {"v": 2, "slots": slots, "market": market})
    assert proc.returncode == 0, proc.stderr + proc.stdout
    results = {r["id"]: r for r in json.loads(proc.stdout)["results"]}

    for slot_id, closed in (("on", True), ("off", False)):
        argv = ["breakout", "BTC", "1h", "--mode=paper", "--market-stdin", "--ohlcv-limit", "200"]
        if closed:
            argv.append("--closed-bar-decisions")
        single = _run(CHECK_HL, argv, {"v": 2, "market": market})
        assert single.returncode == 0, single.stderr
        assert _strip(results[slot_id]) == _strip(json.loads(single.stdout))
    assert results["on"]["signal"] == 1
    assert results["off"]["signal"] == 0
    assert "closed_bar_decision" not in results["off"]


def test_batch_rejects_a_non_boolean_flag():
    bars = _bars(120)
    market = _market({"BTC|1h": _hl_frame(bars)}, cutoff_ms=bars[-1]["t"] + 1)
    slots = [{"id": "a", "strategy": "breakout", "mode": "paper", "closed_bar_decisions": "yes"},
             {"id": "b", "strategy": "breakout", "mode": "paper"}]
    proc = _run(CHECK_HL, ["--batch-check", "--symbol=BTC", "--timeframe=1h", "--market-stdin"],
                {"v": 2, "slots": slots, "market": market})
    assert proc.returncode == 1
    out = json.loads(proc.stdout)
    assert out["error_scope"] == "shared_state"
    assert "closed_bar_decisions must be a JSON boolean" in out["error"]


def test_decision_regime_reads_the_closed_frame_and_protection_keeps_the_injected_regime():
    bars = _bars(240, forming_spike=30.0)
    cutoff = bars[-1]["t"] + H // 2
    injected = '{"default":{"regime":"ranging","score":0.1,"classifier":"adx","metrics":{"adx":5.0}}}'
    extra = ["--regime-enabled", "--regime-windows-spec-json", SPEC, "--ohlcv-limit", "200",
             "--regime-payload-json", injected, "--decision-regime-timeframe=1h"]
    out = _hl_check("breakout", bars, cutoff, closed=True, extra=extra)
    sys.path.insert(0, os.path.join(_REPO_ROOT, "shared_scripts"))
    from check_regime import compute_regime_bundle
    closed = _frame_from_rows(_hl_rows(bars[:-1])[-200:])
    expected = compute_regime_bundle(closed, json.loads(SPEC))["regime"]
    assert out["decision_regime"]["default"]["regime"] == expected["default"]["regime"]
    assert out["decision_regime"]["default"]["metrics"]["adx"] == pytest.approx(expected["default"]["metrics"]["adx"])
    assert out["regime"]["default"]["regime"] == "ranging"


def _fake_ccxt_dir(tmp_path, rows_by_key):
    pkg = tmp_path / "fakes" / "ccxt"
    pkg.mkdir(parents=True)
    fixture = tmp_path / "ccxt_rows.json"
    fixture.write_text(json.dumps(rows_by_key))
    (pkg / "__init__.py").write_text(textwrap.dedent(f"""
        import json

        _ROWS = json.load(open({str(fixture)!r}))


        class Exchange:
            def __init__(self, config=None):
                self.config = config or {{}}

            def fetch_ohlcv(self, symbol, timeframe, since=None, limit=None):
                rows = _ROWS[symbol + "|" + timeframe]
                if rows == "network_error":
                    raise NetworkError("frozen fixture: " + symbol + " " + timeframe + " unreachable")
                return [list(r) for r in rows[-limit:]] if limit else [list(r) for r in rows]

            def fetch_ticker(self, symbol):
                raise RuntimeError("no ticker in the frozen fixture")

            def load_markets(self):
                return {{}}


        class binanceus(Exchange):
            pass


        class okx(Exchange):
            pass


        class RateLimitExceeded(Exception):
            pass


        class NetworkError(Exception):
            pass
    """))
    return str(tmp_path / "fakes")


def _open_time_rows(bars):
    return [[b["t"], b["o"], b["h"], b["l"], b["c"], b["v"]] for b in bars]


CLOCK_MS = T0 + 200 * H + H // 2


def _clock_dir(tmp_path):
    root = tmp_path / "clock"
    root.mkdir()
    (root / "sitecustomize.py").write_text(textwrap.dedent(f"""
        import time

        _wall = time.time
        _start = _wall()
        time.time = lambda: {CLOCK_MS / 1000!r} + (_wall() - _start)
    """))
    return str(root)


def _clock_env(tmp_path, fakes):
    return {"PYTHONPATH": fakes + os.pathsep + _clock_dir(tmp_path)}


def _now_bars(n, **kw):
    start = CLOCK_MS - (CLOCK_MS % H) - (n - 1) * H
    bars = _bars(n, **kw)
    for i, b in enumerate(bars):
        b["t"] = start + i * H
        b["T"] = b["t"] + H - 1
    return bars


def _assert_injected_clock(meta):
    assert CLOCK_MS <= meta["cutoff_ms"] < CLOCK_MS + H // 2


def test_binance_spot_direct_acquisition_uses_opening_time_closure(tmp_path):
    bars = _now_bars(120, spike_at=118)
    fakes = _fake_ccxt_dir(tmp_path, {"BTC/USDT|1h": _open_time_rows(bars)})
    env = _clock_env(tmp_path, fakes)
    on = _run(CHECK_SPOT, ["atr_breakout", "BTC/USDT", "1h", "--mode=paper", "--closed-bar-decisions"], env_extra=env)
    off = _run(CHECK_SPOT, ["atr_breakout", "BTC/USDT", "1h", "--mode=paper"], env_extra=env)
    assert on.returncode == 0, on.stderr + on.stdout
    assert off.returncode == 0, off.stderr + off.stdout
    on, off = json.loads(on.stdout), json.loads(off.stdout)
    meta = on["closed_bar_decision"]
    _assert_injected_clock(meta)
    assert meta["closure_rule"] == "opening_time_fixed_duration"
    assert meta["decision_boundary_ms"] == bars[-2]["t"] + H
    closed_df = _strategies("spot").apply_strategy("atr_breakout", _frame_from_rows(_open_time_rows(bars[:-1])), None)
    assert on["signal"] == int(closed_df["signal"].iloc[-1]) == 1
    assert off["signal"] != on["signal"]
    assert on["indicators"]["atr"] == round(float(closed_df["atr"].iloc[-1]), 6)
    assert on["price"] == off["price"]
    assert "closed_bar_decision" not in off


def test_okx_direct_acquisition_uses_opening_time_closure(tmp_path):
    bars = _now_bars(120, spike_at=118)
    fakes = _fake_ccxt_dir(tmp_path, {"BTC/USDT:USDT|1h": _open_time_rows(bars)})
    env = _clock_env(tmp_path, fakes)
    on = _run(CHECK_OKX, ["breakout", "BTC", "1h", "--mode=paper", "--inst-type=swap", "--closed-bar-decisions"], env_extra=env)
    assert on.returncode == 0, on.stderr + on.stdout
    out = json.loads(on.stdout)
    _assert_injected_clock(out["closed_bar_decision"])
    assert out["closed_bar_decision"]["decision_boundary_ms"] == bars[-2]["t"] + H
    assert out["signal"] == 1
    refused = _run(CHECK_OKX, ["funding_skew", "BTC", "1h", "--mode=paper", "--inst-type=swap", "--closed-bar-decisions"], env_extra=env)
    assert refused.returncode == 1
    assert "funding_skew" in json.loads(refused.stdout)["error"]


def _held_long_tp(open_name):
    refs = json.dumps({"open": {"name": open_name, "params": {}}, "closes": [{"name": "tiered_tp_pct", "params": {}}]})
    return ["--strategy-refs", refs, "--position-side", "long", "--position-avg-cost=50",
            "--position-qty=1", "--position-initial-qty=1"]


def test_binance_spot_htf_fetch_failure_holds_and_keeps_protection(tmp_path):
    bars = _now_bars(120, spike_at=118)
    fakes = _fake_ccxt_dir(tmp_path, {"BTC/USDT|1h": _open_time_rows(bars), "BTC/USDT|4h": "network_error"})
    proc = _run(CHECK_SPOT, ["atr_breakout", "BTC/USDT", "1h", "--mode=paper", "--closed-bar-decisions", "--htf-filter",
                             *_held_long_tp("atr_breakout")], env_extra=_clock_env(tmp_path, fakes))
    assert proc.returncode == 0, proc.stderr + proc.stdout
    out = json.loads(proc.stdout)
    meta = out["closed_bar_decision"]
    assert meta["held"] is True
    assert "BTC/USDT 4h candles failed" in meta["hold_reason"]
    assert out["open_action"] == "none"
    assert out["close_fraction"] == 1.0
    assert out["close_strategy"] == "tiered_tp_pct"


def test_okx_decision_regime_fetch_failure_holds_and_keeps_protection(tmp_path):
    bars = _now_bars(120, spike_at=118)
    fakes = _fake_ccxt_dir(tmp_path, {"BTC/USDT:USDT|1h": _open_time_rows(bars), "BTC/USDT:USDT|4h": "network_error"})
    proc = _run(CHECK_OKX, ["breakout", "BTC", "1h", "--mode=paper", "--inst-type=swap", "--closed-bar-decisions",
                            "--regime-enabled", "--regime-windows-spec-json", SPEC, "--decision-regime-timeframe=4h",
                            *_held_long_tp("breakout")], env_extra=_clock_env(tmp_path, fakes))
    assert proc.returncode == 0, proc.stderr + proc.stdout
    out = json.loads(proc.stdout)
    meta = out["closed_bar_decision"]
    assert meta["held"] is True
    assert "BTC 4h candles failed" in meta["hold_reason"]
    assert out["open_action"] == "none"
    assert out["close_fraction"] == 1.0
    assert out["close_strategy"] == "tiered_tp_pct"


@pytest.mark.parametrize("script,argv,key", [
    (CHECK_SPOT, ["atr_breakout", "BTC/USDT", "1h", "--mode=paper", "--closed-bar-decisions", "--htf-filter"], "BTC/USDT|1h"),
    (CHECK_OKX, ["breakout", "BTC", "1h", "--mode=paper", "--inst-type=swap", "--closed-bar-decisions"], "BTC/USDT:USDT|1h"),
])
def test_primary_fetch_failure_stays_a_check_error(tmp_path, script, argv, key):
    fakes = _fake_ccxt_dir(tmp_path, {key: "network_error"})
    proc = _run(script, argv, env_extra=_clock_env(tmp_path, fakes))
    assert proc.returncode == 1, proc.stderr + proc.stdout
    out = json.loads(proc.stdout)
    assert "unreachable" in out["error"]
    assert "closed_bar_decision" not in out


def _fake_hl_sdk_dir(tmp_path, bars):
    root = tmp_path / "hlfakes" / "hyperliquid"
    root.mkdir(parents=True)
    fixture = tmp_path / "hl_bars.json"
    fixture.write_text(json.dumps(bars))
    (root / "__init__.py").write_text("")
    (root / "info.py").write_text(textwrap.dedent(f"""
        import json

        _BARS = json.load(open({str(fixture)!r}))


        class Info:
            def __init__(self, *args, **kwargs):
                pass

            def candles_snapshot(self, symbol, interval, start, end):
                return [dict(b) for b in _BARS if start <= b["t"] <= end]

            def all_mids(self):
                return {{"BTC": str(_BARS[-1]["c"])}}
    """))
    (root / "exchange.py").write_text("class Exchange:\n    pass\n")
    return str(tmp_path / "hlfakes")


def test_hyperliquid_direct_acquisition_uses_native_close_times(tmp_path):
    bars = _now_bars(120, spike_at=118)
    fakes = _fake_hl_sdk_dir(tmp_path, bars)
    env = _clock_env(tmp_path, fakes)
    on = _run(CHECK_HL, ["breakout", "BTC", "1h", "--mode=paper", "--closed-bar-decisions"], env_extra=env)
    assert on.returncode == 0, on.stderr + on.stdout
    out = json.loads(on.stdout)
    meta = out["closed_bar_decision"]
    _assert_injected_clock(meta)
    assert meta["closure_rule"] == "hyperliquid_native_close"
    assert meta["decision_boundary_ms"] == bars[-2]["t"] + H
    assert out["signal"] == 1


@pytest.mark.parametrize("script", [CHECK_RH, CHECK_TS])
def test_session_venues_refuse_closed_bar_decisions(script):
    proc = _run(script, ["breakout", "BTC", "1h", "--closed-bar-decisions"])
    assert proc.returncode == 1
    assert "closed_bar_decisions is not supported" in json.loads(proc.stdout)["error"]


def test_flag_off_single_check_carries_no_closed_bar_fields():
    bars = _bars(120)
    out = _hl_check("breakout", bars, None, closed=False)
    assert {"closed_bar_decision", "decision_regime"}.isdisjoint(out.keys())


def _htf_bars(n, end_ms, step_ms, base=100.0):
    start = end_ms - n * step_ms
    out = []
    price = base
    for i in range(n):
        price = price + (1.0 if i % 3 else -0.4)
        t = start + i * step_ms
        out.append({"t": t, "T": t + step_ms - 1, "o": price - 0.2, "h": price + 0.8,
                    "l": price - 0.9, "c": price, "v": 50.0 + i})
    return out


def test_higher_timeframe_filter_reads_only_bars_closed_by_the_decision_boundary():
    bars = _bars(240, spike_at=238)
    cutoff = bars[-1]["t"] + H // 2
    boundary = bars[-2]["t"] + H
    four_h = 4 * H
    htf = _htf_bars(70, boundary - (boundary % four_h) + four_h, four_h)
    frames = {"BTC|1h": _hl_frame(bars), "BTC|4h": _hl_frame(htf, interval_ms=four_h)}
    out = _hl_check("breakout", bars, cutoff, closed=True, extra=["--htf-filter"], frames=frames)
    closed_htf = [b for b in htf if b["t"] + four_h <= boundary]
    assert closed_htf[-1]["t"] + four_h <= boundary < htf[-1]["t"] + four_h
    assert out["indicators"]["htf_close"] == round(closed_htf[-1]["c"], 6)
    assert out["closed_bar_decision"]["held"] is False

    short = {"BTC|1h": _hl_frame(bars), "BTC|4h": _hl_frame(htf[-40:], interval_ms=four_h)}
    held = _hl_check("breakout", bars, cutoff, closed=True, extra=["--htf-filter"], frames=short)
    assert held["closed_bar_decision"]["held"] is True
    assert "higher timeframe" in held["closed_bar_decision"]["hold_reason"]
    assert held["signal"] == 0


def test_funding_records_after_the_decision_boundary_cannot_change_the_decision():
    bars = _bars(240)
    cutoff = bars[-1]["t"] + H // 2
    boundary = bars[-2]["t"] + H
    base = [{"time": b["t"], "rate": 0.00001 * ((i % 5) - 2)} for i, b in enumerate(bars[:-1])]
    late = base + [{"time": boundary, "rate": 0.05}, {"time": boundary + 60_000, "rate": -0.05}]

    def run(records):
        market = _market({"BTC|1h": _hl_frame(bars)}, cutoff_ms=cutoff)
        market["funding"] = {"BTC": {"current": 0.0, "avg_7d": 0.0, "has_scalar": False,
                                     "records": records, "has_records": True,
                                     "fetched_at_ms": cutoff, "source": "rest"}}
        proc = _run(CHECK_HL, ["funding_skew", "BTC", "1h", "--mode=paper", "--market-stdin",
                               "--closed-bar-decisions"], {"v": 2, "market": market})
        assert proc.returncode == 0, proc.stderr + proc.stdout
        return json.loads(proc.stdout)

    before = run(base)
    after = run(late)
    assert before["closed_bar_decision"]["held"] is False
    assert _strip(before) == _strip(after)


def test_repeated_checks_of_one_closed_bar_repeat_the_decision_until_the_next_bar_closes():
    bars = _bars(120, spike_at=118)
    first = _hl_check("breakout", bars, bars[-1]["t"] + 60_000, closed=True)
    later = _hl_check("breakout", bars, bars[-1]["t"] + H - 1, closed=True)
    assert first["signal"] == later["signal"] == 1
    assert first["indicators"] == later["indicators"]
    assert first["closed_bar_decision"]["input_sha256"] == later["closed_bar_decision"]["input_sha256"]
    assert first["closed_bar_decision"]["decision_boundary_ms"] == later["closed_bar_decision"]["decision_boundary_ms"]
    refs = json.dumps({"open": {"name": "breakout", "params": {}}})
    holding = _hl_check("breakout", bars, bars[-1]["t"] + H - 1, closed=True,
                        extra=["--strategy-refs", refs, "--position-side", "long", "--position-avg-cost=100",
                               "--position-qty=1", "--position-initial-qty=1"])
    assert holding["open_action"] == "long"
    assert holding["closed_bar_decision"]["decision_boundary_ms"] == first["closed_bar_decision"]["decision_boundary_ms"]
    next_bar = _hl_check("breakout", bars, bars[-1]["t"] + H, closed=True)
    assert next_bar["closed_bar_decision"]["decision_boundary_ms"] == bars[-1]["t"] + H
    assert next_bar["signal"] == 0


def test_a_later_seal_with_a_corrected_closed_bar_decides_on_the_corrected_values():
    bars = _bars(120)
    cutoff = bars[-1]["t"] + H // 2
    corrected = [dict(b) for b in bars]
    corrected[-2]["c"] = bars[-2]["c"] + 0.5
    corrected[-2]["h"] = max(bars[-2]["h"], corrected[-2]["c"] + 0.5)
    first = _hl_check("breakout", bars, cutoff, closed=True)
    later = _hl_check("breakout", corrected, cutoff + 60_000, closed=True)
    assert first["closed_bar_decision"]["bar_open_ms"] == later["closed_bar_decision"]["bar_open_ms"] == bars[-2]["t"]
    socket_df = _strategies().apply_strategy("breakout", _frame_from_rows(_hl_rows(bars[:-1])), None)
    corrected_df = _strategies().apply_strategy("breakout", _frame_from_rows(_hl_rows(corrected[:-1])), None)
    assert first["indicators"]["atr"] == round(float(socket_df["atr"].iloc[-1]), 6)
    assert later["indicators"]["atr"] == round(float(corrected_df["atr"].iloc[-1]), 6)
    assert later["indicators"]["atr"] != first["indicators"]["atr"]


_RESTING_TIERS = [{"atr_multiple": 1.99, "close_fraction": 0.5}, {"atr_multiple": 2.0, "close_fraction": 1.0}]
_RESTING_SPEC = {"taker_fee_pct": 0.0, "maker_fee_pct": 0.0, "half_spread_pct": 0.0, "slippage_pct": 0.0,
                 "size_decimals": 2, "min_notional_usd": 10.0, "min_notional_margin": 0.03}


def _resting_bars(crossing_low):
    flat = [{"t": T0 + i * H, "T": T0 + (i + 1) * H - 1, "o": 100.0, "h": 100.2, "l": 99.8, "c": 100.0, "v": 1000.0}
            for i in range(60)]
    tail = [(100.0, 100.3, 99.8, 100.1), (100.1, 104.0, crossing_low, 103.5),
            (103.5, 103.7, 103.2, 103.4), (103.4, 110.0, 103.0, 109.0)]
    for o, h, low, c in tail:
        t = T0 + len(flat) * H
        flat.append({"t": t, "T": t + H - 1, "o": o, "h": h, "l": low, "c": c, "v": 1000.0})
    return flat


def _resting_paper(bars, stop_trigger_px, mode="paper", batch=False):
    entry = len(bars) - 4
    refs = {"open": {"name": "breakout", "params": {}}, "closes": [{"name": "tiered_tp_atr", "params": {"tp_tiers": _RESTING_TIERS}}]}
    rule = {"v": 1, "k_ticks": 1, "sz_decimals": 2, "entry_time_ms": bars[entry]["t"] + 600_000,
            "stop_trigger_px": stop_trigger_px, "hold_reason": "", "scanned_through_ms": 0, "prior_reach_px": None}
    market = _market({"BTC|1h": _hl_frame(bars)}, cutoff_ms=bars[-1]["t"] + H // 2, mid=bars[-1]["c"])
    if batch:
        slot = {"id": "rule", "strategy": "breakout", "mode": mode, "mode_args": [f"--mode={mode}"], "strategy_refs": refs,
                "position_side": "long", "resting_tp_rule": rule,
                "position_ctx": {"side": "long", "avg_cost": 100.0, "current_quantity": 1.0, "initial_quantity": 1.0, "entry_atr": 2.0}}
        return _run(CHECK_HL, ["--batch-check", "--symbol=BTC", "--timeframe=1h", "--ohlcv-limit", "200", "--atr-method=simple",
                               "--market-stdin"], {"v": 2, "slots": [slot], "market": market})
    argv = ["breakout", "BTC", "1h", f"--mode={mode}", "--market-stdin", "--ohlcv-limit", "200",
            "--strategy-refs", json.dumps(refs), "--position-side", "long", "--position-avg-cost=100",
            "--position-qty=1", "--position-initial-qty=1", "--position-entry-atr=2",
            "--resting-tp-rule-json=" + json.dumps(rule)]
    return _run(CHECK_HL, argv, {"v": 2, "market": market})


def _resting_backtest(bars, stop_loss_atr_mult):
    sys.path.insert(0, os.path.join(_REPO_ROOT, "backtest"))
    from backtester import Backtester
    closed = bars[:-1]
    entry = len(bars) - 4
    df = _frame_from_rows(_hl_rows(closed))
    df["open_action"] = ["long" if i == entry - 1 else "none" for i in range(len(closed))]
    df["atr"] = 2.0
    bt = Backtester(initial_capital=1000.0, platform="hyperliquid", strategy_type="perps",
                    execution_spec=dict(_RESTING_SPEC), stop_loss_atr_mult=stop_loss_atr_mult,
                    close_strategies=[{"name": "tiered_tp_atr", "params": {"tp_tiers": _RESTING_TIERS}}],
                    resting_tp_trade_through=True)
    trades = bt.run(df, strategy_name="breakout", symbol="BTC", timeframe="1h", save=False)["trades"]
    return df, trades


def test_resting_rule_paper_check_and_backtester_book_the_same_tier_close():
    bars = _resting_bars(crossing_low=99.9)
    proc = _resting_paper(bars, stop_trigger_px=90.0)
    assert proc.returncode == 0, proc.stderr + proc.stdout
    paper = json.loads(proc.stdout)
    df, trades = _resting_backtest(bars, stop_loss_atr_mult=5.0)

    assert paper["close_strategy"] == "tiered_tp_atr"
    assert paper["close_fraction"] == 0.5
    assert paper["close_tier_fill_price"] == 103.98
    echo = paper["resting_tp_rule"]
    assert (echo["held"], echo["coverage"], echo["observed_bars"]) == (False, "complete", 3)
    assert echo["entry_bar_open_ms"] == bars[-4]["t"]
    assert echo["last_observed_open_ms"] == bars[-2]["t"]
    assert echo["reach_bar_open_ms"] == bars[-3]["t"]
    first = trades[0]
    assert str(df.index[-2]) == first["exit_date"]
    assert (first["exit_price"], first["shares"] / (first["shares"] + trades[1]["shares"])) == (103.98, 0.5)
    assert first["exit_reason"].startswith("tiered_tp_atr")

    batch = _resting_paper(bars, stop_trigger_px=90.0, batch=True)
    assert batch.returncode == 0, batch.stderr + batch.stdout
    assert _strip(json.loads(batch.stdout)["results"][0]) == _strip(paper)


def test_resting_rule_stop_reached_in_the_crossing_bar_books_no_paper_tier_and_the_backtester_stop():
    bars = _resting_bars(crossing_low=98.0)
    proc = _resting_paper(bars, stop_trigger_px=98.5)
    assert proc.returncode == 0, proc.stderr + proc.stdout
    paper = json.loads(proc.stdout)
    df, trades = _resting_backtest(bars, stop_loss_atr_mult=0.75)

    assert paper["close_fraction"] == 0.0
    assert "close_tier_fill_price" not in paper
    assert paper["resting_tp_rule"]["stop_reached_bar_open_ms"] == bars[-3]["t"]
    assert [(t["exit_date"], t["exit_price"], t["exit_reason"]) for t in trades] == [(str(df.index[-2]), 98.5, "sl")]


@pytest.mark.parametrize("batch", [False, True])
def test_resting_rule_refuses_live_mode(batch):
    proc = _resting_paper(_resting_bars(crossing_low=99.9), stop_trigger_px=90.0, mode="live", batch=batch)
    assert proc.returncode == 1
    out = json.loads(proc.stdout)
    error = out["results"][0]["error"] if batch else out["error"]
    assert "paper-only" in error


def _moving_stop_bars(tail):
    bars = [{"t": T0 + i * H, "T": T0 + (i + 1) * H - 1, "o": 100.0, "h": 100.2, "l": 99.8, "c": 100.0, "v": 1000.0}
            for i in range(60)]
    for o, h, low, c in tail:
        t = T0 + len(bars) * H
        bars.append({"t": t, "T": t + H - 1, "o": o, "h": h, "l": low, "c": c, "v": 1000.0})
    return bars


def _moving_stop_cycle(bars, tiers, *, entry, closed_through, qty, stop, scanned_through=0, prior=None, mid=None):
    frame = bars[:closed_through + 2]
    refs = {"open": {"name": "breakout", "params": {}}, "closes": [{"name": "tiered_tp_atr", "params": {"tp_tiers": tiers}}]}
    rule = {"v": 1, "k_ticks": 1, "sz_decimals": 2, "entry_time_ms": bars[entry]["t"] + 600_000, "stop_trigger_px": stop,
            "hold_reason": "", "scanned_through_ms": scanned_through, "prior_reach_px": prior}
    market = _market({"BTC|1h": _hl_frame(frame)}, cutoff_ms=frame[-1]["t"] + H // 2,
                     mid=frame[-1]["c"] if mid is None else mid)
    argv = ["breakout", "BTC", "1h", "--mode=paper", "--market-stdin", "--ohlcv-limit", "200",
            "--strategy-refs", json.dumps(refs), "--position-side", "long", "--position-avg-cost=100",
            f"--position-qty={qty}", "--position-initial-qty=1", "--position-entry-atr=2",
            "--resting-tp-rule-json=" + json.dumps(rule)]
    proc = _run(CHECK_HL, argv, {"v": 2, "market": market})
    assert proc.returncode == 0, proc.stderr + proc.stdout
    return json.loads(proc.stdout)


def _two_cycle(bars, tiers, second_stop):
    entry = 60
    first = _moving_stop_cycle(bars, tiers, entry=entry, closed_through=entry + 1, qty=1, stop=98.0)
    echo = first["resting_tp_rule"]
    second = _moving_stop_cycle(bars, tiers, entry=entry, closed_through=entry + 2, qty=0.5, stop=second_stop,
                                scanned_through=echo["next_scanned_through_ms"], prior=echo["next_reach_px"])
    return first, second


def test_resting_rule_breakeven_stop_after_tier_one_does_not_block_tier_two_and_matches_the_backtester():
    tiers = [{"atr_multiple": 1.0, "close_fraction": 0.5}, {"atr_multiple": 2.0, "close_fraction": 1.0}]
    bars = _moving_stop_bars([(100.0, 100.3, 99.8, 99.95), (99.95, 102.5, 99.9, 102.2),
                              (102.2, 104.01, 101.0, 103.5), (103.5, 104.2, 103.2, 103.9)])
    first, second = _two_cycle(bars, tiers, second_stop=100.0)

    assert (first["close_fraction"], first["close_tier_fill_price"]) == (0.5, 102.0)
    assert first["resting_tp_rule"]["next_scanned_through_ms"] == bars[61]["t"]
    assert (second["close_fraction"], second["close_tier_fill_price"]) == (1.0, 104.0)
    assert second["resting_tp_rule"]["stop_reached_bar_open_ms"] == 0

    sys.path.insert(0, os.path.join(_REPO_ROOT, "backtest"))
    from backtester import Backtester
    closed = bars[:63]
    df = _frame_from_rows(_hl_rows(closed))
    df["open_action"] = ["long" if i == 59 else "none" for i in range(len(closed))]
    df["atr"] = 2.0
    bt = Backtester(initial_capital=1000.0, platform="hyperliquid", strategy_type="perps",
                    execution_spec=dict(_RESTING_SPEC), stop_loss_atr_mult=1.0,
                    close_strategies=[{"name": "tiered_tp_atr", "params": {"tp_tiers": tiers}}],
                    resting_tp_trade_through=True)
    trades = bt.run(df, strategy_name="breakout", symbol="BTC", timeframe="1h", save=False)["trades"]
    assert [(t["exit_date"], t["exit_price"]) for t in trades] == [(str(df.index[61]), 102.0), (str(df.index[62]), 104.0)]


@pytest.mark.parametrize("crossing_low,want_fraction", [(104.0, 1.0), (102.9, 0.0)])
def test_resting_rule_trailing_stop_tests_each_bar_against_the_stop_armed_when_it_traded(crossing_low, want_fraction):
    tiers = [{"atr_multiple": 1.0, "close_fraction": 0.5}, {"atr_multiple": 3.0, "close_fraction": 1.0}]
    bars = _moving_stop_bars([(100.0, 100.3, 99.8, 100.1), (100.1, 102.5, 101.0, 102.2),
                              (104.5, 106.01, crossing_low, 105.5), (105.5, 105.8, 105.2, 105.6)])
    first, second = _two_cycle(bars, tiers, second_stop=103.0)

    assert (first["close_fraction"], first["close_tier_fill_price"]) == (0.5, 102.0)
    assert second["close_fraction"] == want_fraction
    if want_fraction:
        assert second["close_tier_fill_price"] == 106.0
    else:
        assert second["resting_tp_rule"]["stop_reached_bar_open_ms"] == bars[62]["t"]
        assert second["resting_tp_rule"]["next_scanned_through_ms"] == bars[62]["t"]


_STOP_BAR_TIERS = [{"atr_multiple": 1.0, "close_fraction": 0.5}, {"atr_multiple": 3.0, "close_fraction": 1.0}]


def _stop_bar_bars(bar63_low):
    return _moving_stop_bars([(100.0, 102.3, 99.9, 102.2), (102.2, 103.8, 103.2, 103.6), (103.6, 105.8, 102.9, 105.5),
                              (105.5, 106.01, bar63_low, 105.9), (105.9, 106.0, 105.5, 105.8)])


def _stop_bar_cycles(bars, stops, mids=None):
    out = []
    scanned_through, prior, qty = 0, None, 1.0
    for i, stop in enumerate(stops):
        cycle = _moving_stop_cycle(bars, _STOP_BAR_TIERS, entry=60, closed_through=60 + i, qty=qty, stop=stop,
                                   scanned_through=scanned_through, prior=prior, mid=(mids or {}).get(i))
        echo = cycle["resting_tp_rule"]
        scanned_through, prior = echo["next_scanned_through_ms"], echo["next_reach_px"]
        qty = round(qty * (1 - cycle["close_fraction"]), 8) if cycle["close_fraction"] < 1 else 0.0
        out.append(cycle)
    return out


@pytest.mark.parametrize("bar63_low,want_fraction", [(105.2, 1.0), (102.95, 0.0)])
def test_resting_rule_stop_bar_the_paper_stop_did_not_book_drops_only_that_bar(bar63_low, want_fraction):
    bars = _stop_bar_bars(bar63_low)
    first, second, third, fourth = _stop_bar_cycles(bars, [98.0, 98.0, 103.0, 103.0])

    assert (first["close_fraction"], first["close_tier_fill_price"]) == (0.5, 102.0)
    assert second["close_fraction"] == 0.0
    assert second["resting_tp_rule"]["next_scanned_through_ms"] == bars[61]["t"]
    assert third["close_fraction"] == 0.0
    assert "close_tier_fill_price" not in third
    assert third["resting_tp_rule"]["stop_reached_bar_open_ms"] == bars[62]["t"]
    assert third["resting_tp_rule"]["next_scanned_through_ms"] == bars[62]["t"]
    assert third["resting_tp_rule"]["next_reach_px"] == 103.8
    assert fourth["resting_tp_rule"]["next_scanned_through_ms"] == bars[63]["t"]
    assert fourth["close_fraction"] == want_fraction
    if want_fraction:
        assert fourth["close_tier_fill_price"] == 106.0
        assert fourth["resting_tp_rule"]["stop_reached_bar_open_ms"] == 0
    else:
        assert "close_tier_fill_price" not in fourth
        assert fourth["resting_tp_rule"]["stop_reached_bar_open_ms"] == bars[63]["t"]


def test_resting_rule_stop_bar_with_a_breaching_mark_books_no_tier_from_that_bar():
    bars = _stop_bar_bars(105.2)
    cycles = _stop_bar_cycles(bars, [98.0, 98.0, 103.0], mids={2: 102.8})
    third = cycles[2]
    assert third["close_fraction"] == 0.0
    assert "close_tier_fill_price" not in third
    assert third["resting_tp_rule"]["stop_reached_bar_open_ms"] == bars[62]["t"]


@pytest.mark.parametrize("bar_low,sent_stop,want", [(99.5, 99.0, "tier"), (98.9, 99.0, "sl")])
def test_resting_rule_trailing_stop_ratcheted_inside_a_bar_tests_the_bar_against_the_stop_at_its_open(bar_low, sent_stop, want):
    tiers = [{"atr_multiple": 1.5, "close_fraction": 0.5}, {"atr_multiple": 3.0, "close_fraction": 1.0}]
    bars = _moving_stop_bars([(100.0, 100.2, 99.8, 100.0), (100.0, 103.5, bar_low, 103.0), (103.0, 103.2, 102.8, 103.0)])
    first = _moving_stop_cycle(bars, tiers, entry=60, closed_through=60, qty=1, stop=99.0)
    assert first["close_fraction"] == 0.0
    echo = first["resting_tp_rule"]
    second = _moving_stop_cycle(bars, tiers, entry=60, closed_through=61, qty=1, stop=sent_stop,
                                scanned_through=echo["next_scanned_through_ms"], prior=echo["next_reach_px"])

    sys.path.insert(0, os.path.join(_REPO_ROOT, "backtest"))
    from backtester import Backtester
    closed = bars[:62]
    df = _frame_from_rows(_hl_rows(closed))
    df["open_action"] = ["long" if i == 59 else "none" for i in range(len(closed))]
    df["atr"] = 2.0
    bt = Backtester(initial_capital=1000.0, platform="hyperliquid", strategy_type="perps",
                    execution_spec=dict(_RESTING_SPEC), trailing_stop_pct=0.01,
                    close_strategies=[{"name": "tiered_tp_atr", "params": {"tp_tiers": tiers}}],
                    resting_tp_trade_through=True)
    trades = bt.run(df, strategy_name="breakout", symbol="BTC", timeframe="1h", save=False)["trades"]
    booked = [(t["exit_date"], t["exit_price"], t["exit_reason"].split(":")[0]) for t in trades if t["exit_reason"] != "end_of_data"]
    if want == "tier":
        assert (second["close_fraction"], second["close_tier_fill_price"]) == (0.5, 103.0)
        assert second["resting_tp_rule"]["stop_reached_bar_open_ms"] == 0
        assert booked[0][:2] == (str(df.index[61]), 103.0)
        assert booked[0][2].startswith("tiered_tp_atr")
    else:
        assert second["close_fraction"] == 0.0
        assert second["resting_tp_rule"]["stop_reached_bar_open_ms"] == bars[61]["t"]
        assert booked == [(str(df.index[61]), 99.0, "sl")]


MFI = "money_flow_index_reversal"
MFI_PARAMS = {"lookback": 2, "oversold": 30.0, "overbought": 70.0}


def _set_bar(bar, price, volume):
    return dict(bar, o=price + 0.1, h=price + 0.5, l=price - 0.5, c=price, v=volume)


def _mfi_reversal_bars(n=240, forming_price_jump=5.0, forming_volume=50_000.0):
    bars = _bars(n)
    base = bars[-5]["c"]
    bars[-4] = _set_bar(bars[-4], base - 2.0, 1000.0)
    bars[-3] = _set_bar(bars[-3], base - 4.0, 1000.0)
    bars[-2] = _set_bar(bars[-2], base - 6.0, 1000.0)
    bars[-1] = _set_bar(bars[-1], base - 6.0 + forming_price_jump, forming_volume)
    return bars


def _mfi_refs(closes=None):
    refs = {"open": {"name": MFI, "params": dict(MFI_PARAMS)}}
    if closes:
        refs["closes"] = closes
    return json.dumps(refs)


def test_money_flow_index_forming_bar_cannot_create_a_closed_bar_entry():
    bars = _mfi_reversal_bars()
    cutoff = bars[-1]["t"] + H // 2
    extra = ["--strategy-refs", _mfi_refs()]
    disabled = _hl_check(MFI, bars, cutoff, closed=False, extra=extra)
    enabled = _hl_check(MFI, bars, cutoff, closed=True, extra=extra)
    calm_bars = [dict(b) for b in bars]
    calm_bars[-1] = _set_bar(calm_bars[-1], bars[-2]["c"], 1.0)
    calm = _hl_check(MFI, calm_bars, cutoff, closed=True, extra=extra)
    assert disabled["signal"] == 1 and disabled["open_action"] == "long"
    assert enabled["signal"] == 0 and enabled["open_action"] == "none"
    assert enabled["indicators"] == calm["indicators"]
    assert enabled["closed_bar_decision"]["input_sha256"] == calm["closed_bar_decision"]["input_sha256"]
    assert enabled["closed_bar_decision"]["forming_rows_dropped"] == 1
    closed_df = _strategies().apply_strategy(MFI, _frame_from_rows(_hl_rows(bars[:-1])), MFI_PARAMS)
    last = closed_df.iloc[-1]
    assert enabled["indicators"]["mfi_value"] == float(last["mfi_value"])
    assert enabled["indicators"]["mfi_prev_value"] == float(last["mfi_prev_value"])
    assert enabled["indicators"]["mfi_positive_total"] == float(last["mfi_positive_total"])
    assert enabled["indicators"]["mfi_reason_code"] == 0
    assert enabled["indicators"]["mfi_volume"] == bars[-2]["v"]
    assert enabled["indicators"]["mfi_eval_ts_ms"] == float(bars[-2]["T"])
    assert disabled["indicators"]["mfi_volume"] == bars[-1]["v"]


def test_money_flow_index_closed_bar_batch_matches_individual_checks():
    bars = _mfi_reversal_bars()
    cutoff = bars[-1]["t"] + H // 2
    market = _market({"BTC|1h": _hl_frame(bars)}, cutoff_ms=cutoff, mid=bars[-1]["c"])
    refs = json.loads(_mfi_refs())
    slots = [
        {"id": "on", "strategy": MFI, "mode": "paper", "mode_args": ["--mode=paper"], "strategy_refs": refs,
         "closed_bar_decisions": True},
        {"id": "off", "strategy": MFI, "mode": "paper", "mode_args": ["--mode=paper"], "strategy_refs": refs},
    ]
    proc = _run(CHECK_HL, ["--batch-check", "--symbol=BTC", "--timeframe=1h", "--ohlcv-limit", "200",
                           "--atr-method=simple", "--market-stdin"], {"v": 2, "slots": slots, "market": market})
    assert proc.returncode == 0, proc.stderr + proc.stdout
    results = {r["id"]: r for r in json.loads(proc.stdout)["results"]}
    for slot_id, closed in (("on", True), ("off", False)):
        argv = [MFI, "BTC", "1h", "--mode=paper", "--market-stdin", "--ohlcv-limit", "200",
                "--strategy-refs", _mfi_refs()]
        if closed:
            argv.append("--closed-bar-decisions")
        single = _run(CHECK_HL, argv, {"v": 2, "market": market})
        assert single.returncode == 0, single.stderr
        assert _strip(results[slot_id]) == _strip(json.loads(single.stdout))
    assert results["on"]["signal"] == 0
    assert results["off"]["signal"] == 1


@pytest.mark.parametrize("closed", [True, False])
def test_money_flow_index_invalid_volume_holds_the_entry_and_keeps_protection(closed):
    bars = [dict(b, v=-1.0) for b in _mfi_reversal_bars()]
    cutoff = bars[-1]["t"] + H // 2
    refs = _mfi_refs([{"name": "tiered_tp_pct", "params": {}}])
    held_long = ["--strategy-refs", refs, "--position-side", "long", "--position-avg-cost=50",
                 "--position-qty=1", "--position-initial-qty=1"]
    out = _hl_check(MFI, bars, cutoff, closed=closed, extra=held_long, mid=bars[-1]["c"])
    if closed:
        assert out["closed_bar_decision"]["held"] is True
        assert "negative volume" in out["closed_bar_decision"]["hold_reason"]
        assert out["indicators"] == {}
    else:
        assert out["indicators"]["mfi_reason_code"] == 15
        assert out["indicators"]["mfi_valid"] == 0.0
    assert out["open_action"] == "none"
    assert out["close_strategy"] == "tiered_tp_pct" and out["close_fraction"] == 1.0
    assert out["signal"] == -1
    flat = _hl_check(MFI, bars, cutoff, closed=closed, extra=["--strategy-refs", refs])
    assert flat["signal"] == 0 and flat["open_action"] == "none"

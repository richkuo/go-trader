import pandas as pd
import pytest

import run_backtest


_BUILD_PARSER_REJECTS_UNKNOWN_CHOICE_CASES = [
    ["--platform", "mystery-exchange"],
    ["--registry", "options"],
    ["--optimize-metric", "alpha_decay"],
]


def test_build_parser_rejects_unknown_choice(subtests):
    for i, argv in enumerate(_BUILD_PARSER_REJECTS_UNKNOWN_CHOICE_CASES):
        with subtests.test(case=i, argv=argv):
            parser = run_backtest._build_parser()
            with pytest.raises(SystemExit):
                parser.parse_args(argv)


def _spy_single(monkeypatch):
    seen = {}

    def spy(*args, **kwargs):
        seen.setdefault("calls", []).append(kwargs)
        return None

    monkeypatch.setattr(run_backtest, "run_single_backtest", spy)
    return seen


_MODE_THREADS_DIRECTION_CASES = [
    ["--mode", "single"],
    ["--mode", "compare"],
    ["--mode", "multi", "--symbols", "BTC/USDT"],
]


def test_mode_threads_direction(monkeypatch, subtests):
    for i, extra_argv in enumerate(_MODE_THREADS_DIRECTION_CASES):
        with subtests.test(case=i, extra_argv=extra_argv):
            with monkeypatch.context() as mp:
                seen = _spy_single(mp)
                mp.setattr("sys.argv", [
                    "run_backtest.py", *extra_argv,
                    "--strategy", "sma_crossover", "--direction", "short",
                ])
                run_backtest.main()
                assert seen["calls"][0]["direction"] == "short"


def _wave_frame(n=400):
    import numpy as np
    idx = pd.date_range("2024-01-01", periods=n, freq="1h")
    close = pd.Series(100.0 + 8.0 * np.sin(np.linspace(0, 12 * np.pi, n)) + np.linspace(0, 6, n),
                      index=idx)
    return pd.DataFrame({"open": close.shift(1).fillna(close.iloc[0]), "high": close + 0.5,
                         "low": close - 0.5, "close": close, "volume": 10.0})


def _fee_config(tmp_path, sid, stype, platform=None, **extra):
    import json
    sc = {"id": sid, "type": stype, "args": ["sma_crossover", "BTC/USDT", "1h"],
          "open_strategy": {"name": "sma_crossover",
                            "params": {"fast_period": 5, "slow_period": 20}}}
    if platform:
        sc["platform"] = platform
    sc.update(extra)
    path = tmp_path / f"{sid}.json"
    path.write_text(json.dumps({"config_version": 15, "strategies": [sc]}))
    return str(path)


def _main_config(monkeypatch, capsys, path, sid, *extra, funding=None):
    import backtester
    import funding_fetcher
    frame = _wave_frame()
    data_calls = []
    funding_calls = []

    def load(*a, **k):
        data_calls.append(a)
        return frame.copy()

    def no_funding(*a, **k):
        funding_calls.append(a)
        if funding is None:
            raise AssertionError("unexpected funding history request")
        return funding(*a, **k)

    monkeypatch.setattr(run_backtest, "load_cached_data", load)
    monkeypatch.setattr(backtester, "store_backtest_result", lambda *a, **k: None)
    monkeypatch.setattr(funding_fetcher, "load_cached_funding", no_funding)
    captured = {}
    real = run_backtest.run_single_backtest

    def wrap(*args, **kwargs):
        captured["kwargs"] = kwargs
        captured["result"] = real(*args, **kwargs)
        return captured["result"]

    monkeypatch.setattr(run_backtest, "run_single_backtest", wrap)
    monkeypatch.setattr("sys.argv", [
        "run_backtest.py", "--config", path, "--strategy", sid, "--mode", "single",
        "--symbol", "BTC/USDT", "--timeframe", "1h", "--since", "2024-01-01", *extra])
    code = None
    try:
        run_backtest.main()
    except SystemExit as exc:
        code = exc.code
    out = capsys.readouterr().out
    return {"code": code, "out": out, "kwargs": captured.get("kwargs"),
            "result": captured.get("result"), "data_calls": data_calls,
            "funding_calls": funding_calls, "frame": frame}


def _fee_line(out):
    import re
    lines = [l for l in out.splitlines() if "Fee model:" in l]
    assert len(lines) == 1, out
    m = re.search(r"platform=(\S+) commission_pct=(\S+) slippage_pct=(\S+)", lines[0])
    return m.group(1), float(m.group(2)), float(m.group(3))


def _selection_warnings(out):
    return [l for l in out.splitlines() if l.startswith("[WARN] platform selection")]


def _assert_engine_costs(run, commission, slippage=0.0005):
    trades = run["result"]["trades"]
    opens = run["frame"]["open"]
    assert trades, "fixture must produce fills"
    closed = [t for t in trades if t["exit_reason"]]
    assert closed
    for t in trades:
        sign = 1 if t["side"] == "long" else -1
        raw_entry = float(opens.loc[pd.Timestamp(t["entry_date"])])
        assert t["entry_price"] == pytest.approx(raw_entry * (1 + sign * slippage), rel=1e-12)
        notional = t["shares"] * t["entry_price"]
        assert t["entry_fee"] in (pytest.approx(notional * commission, abs=2e-6),
                                  pytest.approx(notional * commission / (1 - commission), abs=2e-6))
    for t in closed:
        assert t["exit_fee"] == pytest.approx(t["shares"] * t["exit_price"] * commission, abs=2e-6)
        if t["exit_reason"] == "signal":
            sign = 1 if t["side"] == "long" else -1
            raw_exit = float(opens.loc[pd.Timestamp(t["exit_date"])])
            assert t["exit_price"] == pytest.approx(raw_exit * (1 - sign * slippage), rel=1e-12)


_AUTOMATIC_FEE_CASES = [
    ("rh-sma", "spot", None, "robinhood", 0.0),
    ("cfg-rh", "spot", "robinhood", "robinhood", 0.0),
    ("hl-but-rh", "spot", "robinhood", "robinhood", 0.0),
    ("hl-perp", "perps", None, "hyperliquid", 0.00045),
    ("hl-manual", "manual", None, "hyperliquid", 0.00045),
    ("bus-spot", "spot", "binanceus", "binanceus", 0.001),
    ("noprefix-perp", "perps", None, "binanceus", 0.001),
]


def test_config_run_selects_the_strategy_fee_model_in_the_real_engine(monkeypatch, capsys, tmp_path, subtests):
    for sid, stype, platform, want, commission in _AUTOMATIC_FEE_CASES:
        with subtests.test(sid=sid):
            with monkeypatch.context() as mp:
                path = _fee_config(tmp_path, sid, stype, platform)
                run = _main_config(mp, capsys, path, sid, "--funding", "off")
                assert run["code"] is None, run["out"]
                assert run["kwargs"]["platform"] == want
                assert _fee_line(run["out"]) == (want, commission, 0.0005)
                assert _selection_warnings(run["out"]) == []
                assert run["funding_calls"] == []
                _assert_engine_costs(run, commission)


def _manifest(tmp_path, venue="hyperliquid"):
    import hashlib
    import json
    import numpy as np
    import offline_manifest as om
    hour = 3_600_000
    t0 = 1_767_225_600_000
    data = tmp_path / "data"
    data.mkdir()
    n = 400
    close = 100 + 8 * np.sin(np.linspace(0, 12 * np.pi, n)) + np.linspace(0, 6, n)
    open_ = np.concatenate([[close[0]], close[:-1]])
    ts = t0 + hour * np.arange(n)
    om._write_csv_gz(pd.DataFrame({"timestamp": ts, "open": open_,
                                   "high": np.maximum(open_, close) + 0.5,
                                   "low": np.minimum(open_, close) - 0.5,
                                   "close": close, "volume": np.full(n, 10.0)}),
                     str(data / "BTC.csv.gz"))
    om._write_csv_gz(pd.DataFrame({"timestamp": ts + 1, "rate": np.full(n, 0.0001)}),
                     str(data / "BTC_f.csv.gz"))
    (data / "meta.json").write_text(json.dumps({"universe": [{"name": "BTC", "szDecimals": 5}]}))
    quote = "Order MinTradeNtl Order must have minimum value of $10."
    manifest = {
        "schema": om.SCHEMA, "study": "fee-precedence", "venue": venue,
        "provenance": {"kind": "venue", "label": "fixture candles"},
        "interval": "1h", "warmup_bars": 60,
        "windows": {"test": {"start": pd.Timestamp(t0 + hour * 150, unit="ms").isoformat(),
                             "end": pd.Timestamp(t0 + hour * 400, unit="ms").isoformat()}},
        "costs": {"taker_fee_pct": 0.00045, "maker_fee_pct": 0.00015, "slippage_bps": 5.0,
                  "min_notional_usd": 10.0, "min_notional_margin": 0.03},
        "venue_reference": {
            "meta": {"path": "data/meta.json", "sha256": om.sha256_file(data / "meta.json")},
            "documents": [{"url": "https://example.invalid", "quote": quote,
                           "quote_sha256": hashlib.sha256(quote.encode()).hexdigest()}],
        },
        "datasets": [{"coin": "BTC", "size_decimals": 5, "half_spread_bps": 0.5,
                      "candles": {"path": "data/BTC.csv.gz",
                                  "sha256": om.sha256_file(data / "BTC.csv.gz")},
                      "funding": {"path": "data/BTC_f.csv.gz",
                                  "sha256": om.sha256_file(data / "BTC_f.csv.gz")}}],
    }
    path = tmp_path / "manifest.json"
    path.write_text(json.dumps(manifest))
    loaded = om.load_manifest(str(path))
    return str(path), om.execution_spec(loaded, loaded["datasets"][0], 1.0)


_MANIFEST_PRECEDENCE_CASES = [
    ("rh-man", "spot", ()),
    ("rh-man", "spot", ("--platform", "robinhood")),
    ("ibkr-man", "options", ()),
]


def test_manifest_venue_and_costs_win_over_config_and_explicit_platform(monkeypatch, capsys, tmp_path, subtests):
    for i, (sid, stype, extra) in enumerate(_MANIFEST_PRECEDENCE_CASES):
        with subtests.test(sid=sid, extra=extra):
            case = tmp_path / f"case{i}"
            case.mkdir()
            manifest, spec = _manifest(case)
            path = _fee_config(case, sid, stype,
                               close_strategy={"name": "time_stop", "params": {"max_bars": 6}})
            with monkeypatch.context() as mp:
                def never(*a, **k):
                    raise AssertionError("manifest runs must not resolve a config fee platform")
                mp.setattr(run_backtest, "backtest_fee_platform", never)
                mp.setattr(run_backtest, "platform_selection_warning", never)
                run = _main_config(mp, capsys, path, sid, *extra, "--manifest", manifest,
                                   "--manifest-dataset", "BTC 1h", "--manifest-window", "test",
                                   "--comparison-mode", "approximate")
                assert _selection_warnings(run["out"]) == []
                assert run["data_calls"] == []
                assert run["code"] is None, run["out"]
                platform, commission, slippage = _fee_line(run["out"])
                assert platform == "hyperliquid"
                assert commission == spec["taker_fee_pct"]
                assert slippage == pytest.approx(spec["half_spread_pct"] + spec["slippage_pct"], rel=1e-12)


_SEED_CANDLES = """
import math, os, sys
sys.path.insert(0, os.path.join(sys.argv[1], "shared_tools"))
import pandas as pd
from storage import store_ohlcv

start = 1_704_067_200_000
rows = []
prev = None
for i in range(400):
    close = 100.0 + 8.0 * math.sin(i * 12 * math.pi / 399) + 6.0 * i / 399
    open_ = close if prev is None else prev
    rows.append({"timestamp": start + i * 3_600_000, "open": open_, "high": max(open_, close) + 0.5,
                 "low": min(open_, close) - 0.5, "close": close, "volume": 10.0})
    prev = close
store_ohlcv(pd.DataFrame(rows), "binanceus", "BTC/USDT", "1h")
"""


def test_cached_candle_cli_run_charges_robinhood_spot_zero_commission(tmp_path):
    import json
    import os
    import sqlite3
    import subprocess
    import sys

    root = os.path.abspath(os.path.join(os.path.dirname(__file__), "..", ".."))
    db = tmp_path / "cache.db"
    fixture = tmp_path / "offline.json"
    fixture.write_text(json.dumps({"ccxt": {}}))
    env = dict(os.environ)
    env["GO_TRADER_OHLCV_CACHE_DB"] = str(db)
    seed = tmp_path / "seed.py"
    seed.write_text(_SEED_CANDLES)
    subprocess.run([sys.executable, str(seed), root], check=True, env=env,
                   cwd=str(tmp_path), timeout=300)
    env["PYTHONPATH"] = os.path.join(root, "shared_scripts", "testdata", "htf_offline")
    env["HTF_OFFLINE_FIXTURE"] = str(fixture)
    path = _fee_config(tmp_path, "rh-sma", "spot")
    proc = subprocess.run(
        [sys.executable, os.path.join(root, "backtest", "run_backtest.py"),
         "--config", path, "--strategy", "rh-sma", "--mode", "single",
         "--symbol", "BTC/USDT", "--timeframe", "1h", "--since", "2024-01-01"],
        capture_output=True, text=True, cwd=root, env=env, timeout=600)
    out = proc.stdout + proc.stderr
    assert proc.returncode == 0, out
    assert "network blocked" not in out
    assert "fetching from exchange" not in out
    assert _fee_line(proc.stdout) == ("robinhood", 0.0, 0.0005)
    assert _selection_warnings(proc.stdout) == []
    conn = sqlite3.connect(str(db))
    try:
        saved = conn.execute("SELECT trades_json FROM backtest_results").fetchall()
        opens = dict(conn.execute(
            "SELECT timestamp, open FROM ohlcv WHERE timeframe = '1h'").fetchall())
    finally:
        conn.close()
    assert len(saved) == 1
    trades = json.loads(saved[0][0])
    assert trades
    for t in trades:
        raw = opens[int(pd.Timestamp(t["entry_date"]).timestamp() * 1000)]
        assert t["entry_price"] == pytest.approx(raw * 1.0005, rel=1e-12)
        assert t["entry_fee"] == 0.0 and t["exit_fee"] == 0.0
        if t["exit_reason"] == "signal":
            raw_exit = opens[int(pd.Timestamp(t["exit_date"]).timestamp() * 1000)]
            assert t["exit_price"] == pytest.approx(raw_exit * 0.9995, rel=1e-12)

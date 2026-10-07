import json
import types

import numpy as np
import pandas as pd
import pytest

import auto_suggest as asug
import eval_windows as ew
import fee_audit as fa
import monte_carlo as mc
from backtester import Backtester, FundingIncompleteError
from optimizer import walk_forward_optimize
from reporter import format_walk_forward_report

_HOUR_MS = 3_600_000


class _HoldRegistry:
    STRATEGY_REGISTRY = {"hold": {"default_params": {}}}

    @staticmethod
    def list_strategies():
        return ["hold"]

    @staticmethod
    def apply_strategy(name, df, params):
        out = df.copy()
        sig = np.zeros(len(out), dtype=int)
        sig[0] = 1
        out["signal"] = sig
        return out


def _hourly_frame(n=48):
    idx = pd.date_range("2024-06-01", periods=n, freq="h", tz="UTC")
    price = np.full(n, 100.0)
    return pd.DataFrame({
        "open": price, "high": price + 1.0, "low": price - 1.0,
        "close": price, "volume": np.full(n, 1000.0),
    }, index=idx)


def _prints(index, drop_ms=None):
    first = int(pd.Timestamp(index[0]).timestamp() * 1000)
    last = int(pd.Timestamp(index[-1]).timestamp() * 1000)
    rows = []
    hour = (first // _HOUR_MS) * _HOUR_MS
    while hour <= last:
        ts = hour + 76
        if first < ts <= last and ts != drop_ms:
            rows.append((ts, 1e-5))
        hour += _HOUR_MS
    return pd.DataFrame(rows, columns=["timestamp", "rate"])


def _patch_sources(monkeypatch, frame, funding):
    import data_fetcher
    import funding_fetcher

    monkeypatch.setattr(data_fetcher, "load_cached_data",
                        lambda *a, **k: frame.copy(), raising=True)

    def _load(*a, **k):
        return funding.copy()

    monkeypatch.setattr(funding_fetcher, "load_cached_funding", _load, raising=True)


def _row_count(db_path):
    import storage
    conn = storage.get_connection(db_path)
    try:
        return conn.execute("SELECT COUNT(*) FROM backtest_results").fetchone()[0]
    finally:
        conn.close()


def _patch_store(monkeypatch, db_path):
    import backtester as btmod
    import storage
    storage.init_db(db_path)
    saved = db_path

    def _store(result, db_path=None):
        storage.store_backtest_result(result, db_path=saved)

    monkeypatch.setattr(btmod, "store_backtest_result", _store)


def _fifteen_minute_frame():
    idx = pd.date_range("2024-06-01", periods=48 * 4, freq="15min", tz="UTC")
    price = np.full(len(idx), 100.0)
    df = pd.DataFrame({
        "open": price, "high": price + 0.5, "low": price - 0.5,
        "close": price, "volume": np.full(len(idx), 1000.0),
        "signal": np.zeros(len(idx)),
    }, index=idx)
    df.iloc[0, df.columns.get_loc("signal")] = 1
    return df


def _attach(df, funding, mode="charge"):
    import funding_fetcher
    out, _block = funding_fetcher.attach_backtest_funding(
        df, "BTC", "15m", "hyperliquid", "perps", "sma_crossover", mode=mode,
    )
    return out


def test_fee_audit_flags_a_held_hour_with_no_funding_record(monkeypatch):
    frame = _hourly_frame()
    first = int(pd.Timestamp(frame.index[0]).timestamp() * 1000)
    drop = ((first // _HOUR_MS) + 12) * _HOUR_MS + 76
    _patch_sources(monkeypatch, frame, _prints(frame.index, drop_ms=drop))
    leg = fa.screen_leg(_HoldRegistry(), "hold", "BTC/USDT", "1h",
                        ("2024-06-01", None), capital=10000.0,
                        funding_mode="charge")
    assert leg is not None and leg["error"] is None
    assert leg["dataset"] == "BTC/USDT 1h"
    assert leg["funding_incomplete"] is True
    assert leg["funding_mode"] == "charge"


def test_funding_off_default_harnesses_leave_m5_unaccrued(tmp_path, monkeypatch):
    spec = {
        "study": "t",
        "registry": "spot",
        "windows": ["is"],
        "correction": {"method": "benjamini_hochberg", "alpha": 0.05},
        "resamples": 10,
        "seed": 1,
        "candidates": [{"key": "c1", "candidate": {"name": "squeeze_momentum",
                                                   "direction": "long"}}],
    }
    loaded = asug.load_spec(spec, str(tmp_path))
    loaded["funding"] = "off"
    loaded["resamples"] = 10
    loaded["seed"] = 1
    entries = asug.expand_candidates(loaded)
    cmds = asug._dry_run_commands(entries, loaded, str(tmp_path))
    m5 = [c for c in cmds if "fee_audit.py" in c]
    mc_cmds = [c for c in cmds if "monte_carlo.py" in c]
    assert m5 and "--funding off" in m5[0]
    assert mc_cmds == []

    frame = _hourly_frame()
    _patch_sources(monkeypatch, frame, _prints(frame.index))
    import backtester as btmod
    seen = []
    orig = btmod.Backtester.run

    def _spy(self, df, *args, **kwargs):
        seen.append(df)
        return orig(self, df, *args, **kwargs)

    monkeypatch.setattr(btmod.Backtester, "run", _spy)
    leg = fa.screen_leg(_HoldRegistry(), "hold", "BTC/USDT", "1h",
                        ("2024-06-01", None), capital=10000.0,
                        funding_mode="off")
    assert leg["funding_mode"] == "off"
    assert leg["funding_incomplete"] is False
    assert seen and "funding_accrual" not in seen[0].columns
    assert str(seen[0]["funding_mode"].iloc[0]) == "off"

    mc_seen = []

    def _spy_mc(self, df, *args, **kwargs):
        mc_seen.append(df)
        return orig(self, df, *args, **kwargs)

    monkeypatch.setattr(btmod.Backtester, "run", _spy_mc)
    import data_fetcher
    naive = frame.tz_convert("UTC").tz_localize(None)
    monkeypatch.setattr(data_fetcher, "load_cached_data",
                        lambda *a, **k: naive.copy(), raising=True)
    mc.run_leg_trades(
        "sma_crossover", "spot", {"fast_period": 5, "slow_period": 15},
        "BTC/USDT:1h", "is", 1000.0, None, "net", funding_mode="off",
    )
    assert mc_seen and "funding_accrual" not in mc_seen[0].columns
    assert str(mc_seen[0]["funding_mode"].iloc[0]) == "off"


def test_mixed_m1_charge_and_m5_off_exits_1(tmp_path, monkeypatch):
    spec_path = tmp_path / "suggest.json"
    spec_path.write_text(json.dumps({
        "study": "t",
        "registry": "spot",
        "harnesses": ["m1", "m5"],
        "windows": ["is"],
        "candidates": [{"key": "c1", "candidate": {"name": "squeeze_momentum",
                                                   "direction": "long"}}],
    }))

    def _quiet(*_a, **_k):
        return None

    def _run(entry, spec, out_dir, noise_cache, m5_cache):
        entry["results"] = {
            "m1": {"status": "ok", "data": {
                "is": {"verdict": "pass", "funding_mode": "charge"},
            }},
            "m5": {"status": "ok", "data": {
                "salvage_verdict": "healthy",
                "funding_mode": "off",
                "funding_incomplete": False,
            }},
        }
        return entry

    monkeypatch.setattr(asug, "ensure_noise", _quiet)
    monkeypatch.setattr(asug, "ensure_m5", _quiet)
    monkeypatch.setattr(asug, "run_open_entry", _run)
    assert asug.main(["--spec", str(spec_path), "--funding", "off", "--jobs", "1"]) == 1


def _ohlc(n, seed=7):
    rng = np.random.default_rng(seed)
    closes = 100.0 * np.exp(np.cumsum(rng.normal(0.004, 0.01, size=n)))
    idx = pd.date_range("2022-01-01", periods=n, freq="D", tz="UTC")
    return pd.DataFrame({
        "open": closes, "high": closes * 1.01, "low": closes * 0.99,
        "close": closes, "volume": np.full(n, 1000.0),
    }, index=idx)


def _stamp_funding(df, missing):
    out = df.copy()
    block = {
        "mode": "charge", "available": True, "complete": False,
        "coverage": None, "source": "cache", "sha256": None,
        "row_count": 0, "first": None, "last": None,
    }
    out["funding_accrual"] = 0.0
    out["funding_missing_hours"] = np.asarray(missing, dtype=int)
    out["funding_mode"] = "charge"
    out["funding_block_json"] = json.dumps(block, sort_keys=True)
    return out


def test_every_funding_gap_fold_names_funding_in_the_error():
    n = 200
    df = _stamp_funding(_ohlc(n), np.ones(n, dtype=int))
    result = walk_forward_optimize(
        df, "sma_crossover", {"fast_period": [5], "slow_period": [15]},
        n_splits=2, train_pct=0.7, initial_capital=1000.0,
        platform="hyperliquid", verbose=False,
    )
    assert "incomplete funding" in result["error"]
    assert result["funding_skipped_folds"] >= 1
    text = format_walk_forward_report(result)
    assert "incomplete funding" in text
    assert f"Funding-skipped folds: {result['funding_skipped_folds']}" in text


def test_one_funding_skipped_fold_lowers_the_valid_count():
    n = 400
    missing = np.zeros(n, dtype=int)
    missing[: n // 2] = 1
    df = _stamp_funding(_ohlc(n, seed=11), missing)
    result = walk_forward_optimize(
        df, "sma_crossover", {"fast_period": [5], "slow_period": [15]},
        n_splits=2, train_pct=0.7, initial_capital=1000.0,
        platform="hyperliquid", verbose=False,
    )
    assert "error" not in result
    assert result["n_splits"] == 2
    assert result["n_valid_folds"] == result["n_splits"] - 1
    assert result["funding_skipped_folds"] == 1


def test_subhour_prints_at_hour_plus_76ms_save_with_no_unpriced_hours(tmp_path, monkeypatch):
    import funding_fetcher
    df = _fifteen_minute_frame()
    funding = _prints(df.index)
    monkeypatch.setattr(funding_fetcher, "load_cached_funding",
                        lambda *a, **k: funding.copy(), raising=True)
    db = str(tmp_path / "bt.sqlite")
    _patch_store(monkeypatch, db)
    before = _row_count(db)
    attached = _attach(df.drop(columns=["signal"]), funding)
    signals = df.copy()
    for col in ("funding_accrual", "funding_missing_hours", "funding_mode",
                "funding_block_json"):
        if col in attached.columns:
            signals[col] = attached[col].to_numpy()
    bt = Backtester(initial_capital=10000.0, platform="hyperliquid",
                    commission_pct=0.0, slippage_pct=0.0)
    result = bt.run(signals, strategy_name="sma_crossover", symbol="BTC/USDT",
                    timeframe="15m", save=True)
    assert result["funding_unpriced_held_hours"] == 0
    assert _row_count(db) == before + 1


def test_empty_funding_source_refuses_charge_before_save(tmp_path, monkeypatch):
    import funding_fetcher
    df = _fifteen_minute_frame()
    empty = pd.DataFrame(columns=["timestamp", "rate"])
    monkeypatch.setattr(funding_fetcher, "load_cached_funding",
                        lambda *a, **k: empty.copy(), raising=True)
    db = str(tmp_path / "bt.sqlite")
    _patch_store(monkeypatch, db)
    before = _row_count(db)
    attached = _attach(df.drop(columns=["signal"]), empty)
    signals = df.copy()
    for col in ("funding_accrual", "funding_missing_hours", "funding_mode",
                "funding_block_json"):
        if col in attached.columns:
            signals[col] = attached[col].to_numpy()
    bt = Backtester(initial_capital=10000.0, platform="hyperliquid",
                    commission_pct=0.0, slippage_pct=0.0)
    with pytest.raises(FundingIncompleteError) as exc:
        bt.run(signals, strategy_name="sma_crossover", symbol="BTC/USDT",
               timeframe="15m", save=True)
    assert exc.value.metrics["funding_unpriced_held_hours"] > 0
    assert _row_count(db) == before


def test_partial_close_of_half_moves_half_the_funding():
    n = 8
    idx = pd.date_range("2024-01-01", periods=n, freq="D")
    close = np.array([100.0, 100.0, 100.0, 100.0, 110.0, 110.0, 110.0, 110.0])
    df = pd.DataFrame({
        "open": close, "high": close + 1.0, "low": close - 1.0,
        "close": close, "volume": np.full(n, 1000.0),
        "signal": np.zeros(n),
        "funding_accrual": np.array([0.0, 0.01, 0.01, 0.01, 0.0, 0.0, 0.0, 0.0]),
    }, index=idx)
    df.iloc[0, df.columns.get_loc("signal")] = 1
    bt = Backtester(
        initial_capital=10000.0, platform="binanceus",
        commission_pct=0.0, slippage_pct=0.0,
        close_strategies=[{
            "name": "tiered_tp_pct",
            "params": {"tp_tiers": [{"profit_pct": 0.05, "close_fraction": 0.5}]},
        }],
    )
    result = bt.run(df, strategy_name="x", symbol="BTC/USDT", timeframe="1d",
                    save=False)
    closed = [t for t in result["trades"] if t.get("exit_reason") != "end_of_data"]
    rest = [t for t in result["trades"] if t.get("exit_reason") == "end_of_data"]
    assert len(closed) == 1 and len(rest) == 1
    assert closed[0]["funding_pnl"] == pytest.approx(rest[0]["funding_pnl"])
    assert closed[0]["funding_pnl"] * 2 == pytest.approx(result["total_funding_pnl"])
    samples = ew.trade_samples_from_results(result)
    assert len(samples) == len(result["trades"])
    for trade, sample in zip(result["trades"], samples):
        notional = trade["shares"] * trade["entry_price"]
        price_net = trade["pnl"] / notional * 100.0
        delta = trade["funding_pnl"] / notional * 100.0
        assert sample["pnl_pct_net"] == pytest.approx(round(price_net + delta, 6))
    positions = ew.positions_from_results(result)
    assert len(positions) == 1
    assert positions[0]["net_pnl"] == pytest.approx(
        sum(t["pnl"] + t["funding_pnl"] for t in result["trades"]), abs=1e-4)
    assert sum(t["funding_pnl"] for t in result["trades"]) == pytest.approx(
        result["total_funding_pnl"])
    assert mc._leg_returns({"trade_samples": samples}, "net") == [
        s["pnl_pct_net"] for s in samples]


def test_long_held_across_prints_net_falls_by_negative_funding(monkeypatch):
    frame = _hourly_frame(48)
    frame["signal"] = 0
    frame.iloc[0, frame.columns.get_loc("signal")] = 1
    funding = _prints(frame.index)
    _patch_sources(monkeypatch, frame, funding)
    charged_df = _attach(frame, funding, mode="charge")
    off_df = _attach(frame, funding, mode="off")

    def _run(df):
        bt = Backtester(initial_capital=10000.0, platform="hyperliquid",
                        commission_pct=0.0, slippage_pct=0.0)
        return bt.run(df, strategy_name="sma_crossover", symbol="BTC/USDT",
                      timeframe="1h", save=False)

    charged = _run(charged_df)
    off = _run(off_df)
    assert len(charged["trades"]) == 1 and len(off["trades"]) == 1
    trade = charged["trades"][0]
    off_trade = off["trades"][0]
    assert trade["funding_pnl"] < 0
    assert "funding_pnl" not in off_trade
    charged_sample = ew.trade_samples_from_results(charged)[0]
    off_sample = ew.trade_samples_from_results(off)[0]
    notional = trade["shares"] * trade["entry_price"]
    delta = trade["funding_pnl"] / notional * 100.0
    assert charged_sample["pnl_pct_net"] == pytest.approx(
        off_sample["pnl_pct_net"] + delta, abs=1e-6)
    assert ew.positions_from_results(charged)[0]["net_pnl"] == pytest.approx(
        trade["pnl"] + trade["funding_pnl"], abs=1e-4)
    assert mc.trade_returns([trade]) == [pytest.approx(charged_sample["pnl_pct_net"])]
    assert mc.trade_returns([off_trade]) == [pytest.approx(off_sample["pnl_pct_net"])]
    assert mc._leg_returns(
        {"trade_samples": [charged_sample]}, "net") == [charged_sample["pnl_pct_net"]]


def _metrics_frame():
    idx = pd.date_range("2024-01-01", periods=5, freq="D")
    equity = pd.DataFrame({"equity": np.linspace(1000.0, 1100.0, 5)}, index=idx)
    df = pd.DataFrame({"close": np.full(5, 100.0)}, index=idx)
    return equity, df


def test_price_win_with_larger_funding_cost_counts_as_loss():
    flipped = types.SimpleNamespace(
        pnl=1.0, shares=1.0, entry_price=100.0, funding_pnl=-3.0)
    equity, df = _metrics_frame()
    bt = Backtester(initial_capital=1000.0)
    alone = bt._calculate_metrics(equity, [flipped], df)
    assert alone["win_rate"] == 0.0
    assert alone["avg_win_pct"] == 0.0
    assert alone["avg_loss_pct"] == pytest.approx(-2.0)

    winner = types.SimpleNamespace(
        pnl=4.0, shares=1.0, entry_price=100.0, funding_pnl=0.0)
    both = bt._calculate_metrics(equity, [winner, flipped], df)
    assert both["win_rate"] == 50.0
    assert both["profit_factor"] == pytest.approx(2.0)
    assert both["avg_win_pct"] == pytest.approx(4.0)
    assert both["avg_loss_pct"] == pytest.approx(-2.0)


def test_metrics_without_funding_match_price_pnl(monkeypatch):
    bare = [
        types.SimpleNamespace(pnl=80.0, shares=1.0, entry_price=100.0),
        types.SimpleNamespace(pnl=-20.0, shares=1.0, entry_price=100.0),
    ]
    zeroed = [
        types.SimpleNamespace(pnl=80.0, shares=1.0, entry_price=100.0, funding_pnl=0.0),
        types.SimpleNamespace(pnl=-20.0, shares=1.0, entry_price=100.0, funding_pnl=0.0),
    ]
    equity, df = _metrics_frame()
    bt = Backtester(initial_capital=1000.0)
    bare_m = bt._calculate_metrics(equity, bare, df)
    zero_m = bt._calculate_metrics(equity, zeroed, df)
    for key in ("win_rate", "profit_factor", "avg_win_pct", "avg_loss_pct"):
        assert bare_m[key] == zero_m[key]
    assert bare_m["win_rate"] == 50.0
    assert bare_m["profit_factor"] == pytest.approx(round(80.0 / 20.0, 3))
    assert bare_m["avg_win_pct"] == pytest.approx(80.0)
    assert bare_m["avg_loss_pct"] == pytest.approx(-20.0)

    frame = _hourly_frame(48)
    frame["signal"] = 0
    frame.iloc[0, frame.columns.get_loc("signal")] = 1
    funding = _prints(frame.index)
    _patch_sources(monkeypatch, frame, funding)
    off = Backtester(
        initial_capital=10000.0, platform="hyperliquid",
        commission_pct=0.0, slippage_pct=0.0,
    ).run(_attach(frame, funding, mode="off"), strategy_name="sma_crossover",
          symbol="BTC/USDT", timeframe="1h", save=False)
    trades = off["trades"]
    assert trades and all("funding_pnl" not in t for t in trades)
    wins = [t for t in trades if t["pnl"] > 0]
    losses = [t for t in trades if t["pnl"] <= 0]
    assert off["win_rate"] == pytest.approx(round(len(wins) / len(trades) * 100, 2))
    gross_loss = abs(sum(t["pnl"] for t in losses))
    if gross_loss > 0:
        expected_pf = sum(t["pnl"] for t in wins) / gross_loss
        assert off["profit_factor"] == pytest.approx(round(expected_pf, 3))
    else:
        assert off["profit_factor"] is None

    def _pct(group):
        if not group:
            return 0.0
        vals = [t["pnl"] / (t["shares"] * t["entry_price"]) for t in group]
        return round(float(np.mean(vals)) * 100, 2)

    assert off["avg_win_pct"] == pytest.approx(_pct(wins))
    assert off["avg_loss_pct"] == pytest.approx(_pct(losses))


def test_funding_off_delta_neutral_trade_stats_stay_on_price_pnl():
    n = 6
    idx = pd.date_range("2024-01-01", periods=n, freq="D")
    close = np.array([100.0, 100.0, 100.0, 100.0, 100.0, 101.0])
    accrual = np.array([0.0, 0.02, 0.02, 0.02, 0.02, 0.0])

    def _frame(mode):
        df = pd.DataFrame({
            "open": close, "high": close + 1.0, "low": close - 1.0,
            "close": close, "volume": np.full(n, 1000.0),
            "signal": np.zeros(n),
            "funding_accrual": accrual,
        }, index=idx)
        df.iloc[0, df.columns.get_loc("signal")] = 1
        if mode is not None:
            df["funding_mode"] = mode
        return df

    def _run(df):
        return Backtester(
            initial_capital=10000.0, platform="hyperliquid",
            commission_pct=0.0, slippage_pct=0.0,
        ).run(df, strategy_name="delta_neutral_funding", symbol="BTC/USDT",
              timeframe="1d", save=False)

    off = _run(_frame("off"))
    charged = _run(_frame("charge"))
    price_only = _run(_frame(None).drop(columns=["funding_accrual"]))
    assert len(off["trades"]) == 1 and len(charged["trades"]) == 1
    trade = off["trades"][0]
    charged_trade = charged["trades"][0]
    assert trade["pnl"] > 0
    assert "funding_pnl" not in trade
    assert off["total_funding_pnl"] < 0
    assert trade["pnl"] + off["total_funding_pnl"] < 0
    assert off["final_capital"] < price_only["final_capital"]
    for key in ("win_rate", "profit_factor", "avg_win_pct", "avg_loss_pct"):
        assert off[key] == price_only[key]
    assert off["win_rate"] == 100.0
    assert off["profit_factor"] is None
    assert charged_trade["funding_pnl"] < 0
    assert charged_trade["pnl"] + charged_trade["funding_pnl"] < 0
    assert charged["win_rate"] == 0.0
    sample = ew.trade_samples_from_results(off)[0]
    price_pct = trade["pnl"] / (trade["shares"] * trade["entry_price"]) * 100.0
    assert sample["pnl_pct_net"] == pytest.approx(round(price_pct, 6))
    assert mc.trade_returns([trade]) == [pytest.approx(sample["pnl_pct_net"])]


def _real_hl_adapter(info):
    import importlib.util
    import os
    path = os.path.join(os.path.dirname(os.path.dirname(os.path.dirname(os.path.abspath(__file__)))),
                        "platforms", "hyperliquid", "adapter.py")
    spec = importlib.util.spec_from_file_location("_hl_adapter_charge_1747_test", path)
    module = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(module)
    adapter = object.__new__(module.HyperliquidExchangeAdapter)
    adapter._info = info
    return adapter


class _VenueInfo:

    def __init__(self, times):
        self.times = sorted(int(t) for t in times)
        self.fail = False

    def funding_history(self, name, startTime, endTime=None):
        if self.fail:
            raise ConnectionError("venue down")
        return [{"coin": name, "fundingRate": "0.00001", "premium": "0", "time": t}
                for t in self.times if t >= startTime][:500]


def _venue_prints(skip_ms):
    start = int(pd.Timestamp("2024-05-31", tz="UTC").timestamp() * 1000)
    return [start + h * _HOUR_MS + 76 for h in range(96) if start + h * _HOUR_MS + 76 != skip_ms]


def _held_charge_run(df, attached):
    signals = df.copy()
    for col in ("funding_accrual", "funding_missing_hours", "funding_mode",
                "funding_block_json"):
        if col in attached.columns:
            signals[col] = attached[col].to_numpy()
    bt = Backtester(initial_capital=10000.0, platform="hyperliquid",
                    commission_pct=0.0, slippage_pct=0.0)
    return bt.run(signals, strategy_name="sma_crossover", symbol="BTC/USDT",
                  timeframe="15m", save=True)


def test_venue_absent_hour_charges_zero_and_saves(tmp_path, monkeypatch):
    import funding_fetcher
    df = _fifteen_minute_frame()
    gap_hour = int(pd.Timestamp("2024-06-01 12:00", tz="UTC").timestamp() * 1000)
    adapter = _real_hl_adapter(_VenueInfo(_venue_prints(gap_hour + 76)))
    fdb = str(tmp_path / "funding.sqlite")
    db = str(tmp_path / "bt.sqlite")
    _patch_store(monkeypatch, db)
    before = _row_count(db)
    attached, block = funding_fetcher.attach_backtest_funding(
        df.drop(columns=["signal"]), "BTC", "15m", "hyperliquid", "perps", "sma_crossover",
        mode="charge", adapter=adapter, db_path=fdb)
    assert block["available"] is True and block["complete"] is True
    assert block["coverage"]["venue_absent_hours"] == 1
    assert block["coverage"]["missing_hours"] == 0
    assert "fetch_error" not in block
    assert block["venue_absent_sha256"] == funding_fetcher.venue_absent_sha256([gap_hour])
    first = int(pd.Timestamp(df.index[0]).timestamp() * 1000)
    last = int(pd.Timestamp(df.index[-1]).timestamp() * 1000)
    real = [t for t in _venue_prints(gap_hour + 76) if first < t <= last]
    assert block["row_count"] == len(real)
    assert attached["funding_accrual"].sum() == pytest.approx(1e-5 * len(real))
    gap_bar = attached.index.get_loc(pd.Timestamp(gap_hour + 15 * 60 * 1000, unit="ms", tz="UTC"))
    assert attached["funding_accrual"].iloc[gap_bar] == 0.0
    assert int(attached["funding_missing_hours"].sum()) == 0
    result = _held_charge_run(df, attached)
    assert result["funding_unpriced_held_hours"] == 0
    assert _row_count(db) == before + 1
    import storage
    conn = storage.get_connection(db)
    try:
        saved = json.loads(conn.execute(
            "SELECT funding_json FROM backtest_results ORDER BY id DESC LIMIT 1").fetchone()[0])
    finally:
        conn.close()
    assert saved["coverage"]["venue_absent_hours"] == 1


def test_failed_refill_keeps_the_hour_missing_and_refuses_charge(tmp_path, monkeypatch):
    import funding_fetcher
    import storage
    df = _fifteen_minute_frame()
    gap_hour = int(pd.Timestamp("2024-06-01 12:00", tz="UTC").timestamp() * 1000)
    fdb = str(tmp_path / "funding.sqlite")
    seeded = _venue_prints(gap_hour + 76)
    storage.store_funding_rates([{"time": t, "rate": 1e-5} for t in seeded],
                                "hyperliquid", "BTC", db_path=fdb)
    storage.store_funding_coverage("hyperliquid", "BTC", seeded[0], seeded[-1], db_path=fdb)
    info = _VenueInfo(_venue_prints(None))
    info.fail = True
    db = str(tmp_path / "bt.sqlite")
    _patch_store(monkeypatch, db)
    before = _row_count(db)
    attached, block = funding_fetcher.attach_backtest_funding(
        df.drop(columns=["signal"]), "BTC", "15m", "hyperliquid", "perps", "sma_crossover",
        mode="charge", adapter=_real_hl_adapter(info), db_path=fdb)
    assert block["source"] == "cache"
    assert "venue down" in block["fetch_error"]
    assert block["coverage"]["missing_hours"] >= 1
    assert block["coverage"]["venue_absent_hours"] == 0
    assert block["complete"] is False
    assert storage.load_funding_venue_gaps("hyperliquid", "BTC", db_path=fdb) == []
    with pytest.raises(FundingIncompleteError):
        _held_charge_run(df, attached)
    assert _row_count(db) == before
